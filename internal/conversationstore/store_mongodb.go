package conversationstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goccy/go-json"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"smartrouter/internal/core"
)

// MongoDBStore persists conversations in a MongoDB collection.
type MongoDBStore struct {
	coll *mongo.Collection
}

// mongoConversation is the BSON document shape stored in MongoDB.
// conversation is stored as a single JSON string.
// items is stored as an array of JSON strings (each element is a serialized item),
// which enables atomic $push appends without read-modify-write.
type mongoConversation struct {
	TenantID     string    `bson:"tenant_id"`
	ID           string    `bson:"id"`
	Conversation string    `bson:"conversation"`
	Items        []string  `bson:"items"`
	UserPath     string    `bson:"user_path"`
	RequestID    string    `bson:"request_id"`
	StoredAt     time.Time `bson:"stored_at"`
	ExpiresAt    time.Time `bson:"expires_at"`
}

// NewMongoDBStore creates a new conversation store backed by a MongoDB
// collection. It ensures a unique compound index on (tenant_id, id).
func NewMongoDBStore(ctx context.Context, db *mongo.Database) (*MongoDBStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database is required")
	}
	coll := db.Collection("conversations")
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return nil, fmt.Errorf("create conversations index: %w", err)
	}
	return &MongoDBStore{coll: coll}, nil
}

func tenantFilter(tenantID, id string) bson.D {
	return bson.D{{Key: "tenant_id", Value: tenantID}, {Key: "id", Value: id}}
}

func (s *MongoDBStore) toBSON(c *StoredConversation) (bson.D, error) {
	convJSON, err := json.Marshal(c.Conversation)
	if err != nil {
		return nil, fmt.Errorf("marshal conversation: %w", err)
	}

	itemStrings := make([]string, len(c.Items))
	for i, raw := range c.Items {
		itemStrings[i] = string(raw)
	}

	return bson.D{
		{Key: "tenant_id", Value: c.TenantID},
		{Key: "id", Value: c.Conversation.ID},
		{Key: "conversation", Value: string(convJSON)},
		{Key: "items", Value: itemStrings},
		{Key: "user_path", Value: c.UserPath},
		{Key: "request_id", Value: c.RequestID},
		{Key: "stored_at", Value: c.StoredAt},
		{Key: "expires_at", Value: c.ExpiresAt},
	}, nil
}

func (s *MongoDBStore) fromBSON(doc bson.Raw) (*StoredConversation, error) {
	var mdoc mongoConversation
	if err := bson.Unmarshal(doc, &mdoc); err != nil {
		return nil, fmt.Errorf("decode conversation document: %w", err)
	}

	var conv core.Conversation
	if err := json.Unmarshal([]byte(mdoc.Conversation), &conv); err != nil {
		return nil, fmt.Errorf("unmarshal conversation: %w", err)
	}

	items := make([]json.RawMessage, len(mdoc.Items))
	for i, s := range mdoc.Items {
		items[i] = json.RawMessage(s)
	}

	return &StoredConversation{
		Conversation: &conv,
		Items:        items,
		UserPath:     mdoc.UserPath,
		RequestID:    mdoc.RequestID,
		TenantID:     mdoc.TenantID,
		StoredAt:     mdoc.StoredAt,
		ExpiresAt:    mdoc.ExpiresAt,
	}, nil
}

// Create inserts a new conversation snapshot into MongoDB.
func (s *MongoDBStore) Create(ctx context.Context, tenantID string, conversation *StoredConversation) error {
	if conversation == nil || conversation.Conversation == nil || conversation.Conversation.ID == "" {
		return fmt.Errorf("conversation id is required")
	}

	c, err := cloneConversation(conversation)
	if err != nil {
		return err
	}
	c.TenantID = tenantID

	doc, err := s.toBSON(c)
	if err != nil {
		return err
	}

	if _, err := s.coll.InsertOne(ctx, doc); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("conversation already exists: %s", c.Conversation.ID)
		}
		return fmt.Errorf("create conversation: %w", err)
	}
	return nil
}

// Get retrieves a conversation snapshot by tenant and id.
func (s *MongoDBStore) Get(ctx context.Context, tenantID, id string) (*StoredConversation, error) {
	result := s.coll.FindOne(ctx, tenantFilter(tenantID, id))
	if err := result.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get conversation: %w", err)
	}

	doc, err := result.Raw()
	if err != nil {
		return nil, fmt.Errorf("get conversation raw: %w", err)
	}
	return s.fromBSON(doc)
}

// Update replaces an existing conversation snapshot. It uses $set to replace
// all top-level fields.
func (s *MongoDBStore) Update(ctx context.Context, tenantID string, conversation *StoredConversation) error {
	if conversation == nil || conversation.Conversation == nil || conversation.Conversation.ID == "" {
		return fmt.Errorf("conversation id is required")
	}

	c, err := cloneConversation(conversation)
	if err != nil {
		return err
	}
	c.TenantID = tenantID

	updateDoc, err := s.toBSON(c)
	if err != nil {
		return err
	}

	result, err := s.coll.UpdateOne(ctx, tenantFilter(tenantID, c.Conversation.ID), bson.D{
		{Key: "$set", Value: updateDoc},
	})
	if err != nil {
		return fmt.Errorf("update conversation: %w", err)
	}
	if result.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendItems atomically appends items to the items array using $push with $each.
func (s *MongoDBStore) AppendItems(ctx context.Context, tenantID, id string, items []json.RawMessage) error {
	if len(items) == 0 {
		return nil
	}

	each := make(bson.A, len(items))
	for i, item := range items {
		each[i] = string(core.CloneRawJSON(item))
	}

	result, err := s.coll.UpdateOne(ctx, tenantFilter(tenantID, id), bson.D{
		{Key: "$push", Value: bson.D{{Key: "items", Value: bson.D{{Key: "$each", Value: each}}}}},
	})
	if err != nil {
		return fmt.Errorf("append items: %w", err)
	}
	if result.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a conversation snapshot by tenant and id.
func (s *MongoDBStore) Delete(ctx context.Context, tenantID, id string) error {
	result, err := s.coll.DeleteOne(ctx, tenantFilter(tenantID, id))
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	if result.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// Close is a no-op for MongoDB (connection management is external).
func (s *MongoDBStore) Close() error {
	return nil
}
