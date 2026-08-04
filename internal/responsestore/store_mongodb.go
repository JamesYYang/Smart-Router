package responsestore

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

// MongoDBStore persists response snapshots in a MongoDB collection.
type MongoDBStore struct {
	coll *mongo.Collection
}

// mongoResponse is the BSON document shape stored in MongoDB.
// response is stored as a single JSON string.
// input_items is stored as an array of JSON strings.
type mongoResponse struct {
	TenantID           string    `bson:"tenant_id"`
	ID                 string    `bson:"id"`
	Response           string    `bson:"response"`
	InputItems         []string  `bson:"input_items"`
	Provider           string    `bson:"provider"`
	ProviderName       string    `bson:"provider_name"`
	ProviderResponseID string    `bson:"provider_response_id"`
	RequestID          string    `bson:"request_id"`
	UserPath           string    `bson:"user_path"`
	WorkflowVersionID  string    `bson:"workflow_version_id"`
	StoredAt           time.Time `bson:"stored_at"`
	ExpiresAt          time.Time `bson:"expires_at"`
}

// NewMongoDBStore creates a new response store backed by a MongoDB
// collection. It ensures a unique compound index on (tenant_id, id).
func NewMongoDBStore(ctx context.Context, db *mongo.Database) (*MongoDBStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database is required")
	}
	coll := db.Collection("stored_responses")
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "tenant_id", Value: 1}, {Key: "id", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		return nil, fmt.Errorf("create stored_responses index: %w", err)
	}
	return &MongoDBStore{coll: coll}, nil
}

func tenantFilter(tenantID, id string) bson.D {
	return bson.D{{Key: "tenant_id", Value: tenantID}, {Key: "id", Value: id}}
}

func (s *MongoDBStore) toBSON(c *StoredResponse) (bson.D, error) {
	responseJSON, err := json.Marshal(c.Response)
	if err != nil {
		return nil, fmt.Errorf("marshal response: %w", err)
	}

	inputItemStrings := make([]string, len(c.InputItems))
	for i, raw := range c.InputItems {
		inputItemStrings[i] = string(raw)
	}

	return bson.D{
		{Key: "tenant_id", Value: c.TenantID},
		{Key: "id", Value: c.Response.ID},
		{Key: "response", Value: string(responseJSON)},
		{Key: "input_items", Value: inputItemStrings},
		{Key: "provider", Value: c.Provider},
		{Key: "provider_name", Value: c.ProviderName},
		{Key: "provider_response_id", Value: c.ProviderResponseID},
		{Key: "request_id", Value: c.RequestID},
		{Key: "user_path", Value: c.UserPath},
		{Key: "workflow_version_id", Value: c.WorkflowVersionID},
		{Key: "stored_at", Value: c.StoredAt},
		{Key: "expires_at", Value: c.ExpiresAt},
	}, nil
}

func (s *MongoDBStore) fromBSON(doc bson.Raw) (*StoredResponse, error) {
	var mdoc mongoResponse
	if err := bson.Unmarshal(doc, &mdoc); err != nil {
		return nil, fmt.Errorf("decode response document: %w", err)
	}

	var resp core.ResponsesResponse
	if err := json.Unmarshal([]byte(mdoc.Response), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	inputItems := make([]json.RawMessage, len(mdoc.InputItems))
	for i, s := range mdoc.InputItems {
		inputItems[i] = json.RawMessage(s)
	}

	return &StoredResponse{
		Response:           &resp,
		InputItems:         inputItems,
		Provider:           mdoc.Provider,
		ProviderName:       mdoc.ProviderName,
		ProviderResponseID: mdoc.ProviderResponseID,
		RequestID:          mdoc.RequestID,
		UserPath:           mdoc.UserPath,
		WorkflowVersionID:  mdoc.WorkflowVersionID,
		TenantID:           mdoc.TenantID,
		StoredAt:           mdoc.StoredAt,
		ExpiresAt:          mdoc.ExpiresAt,
	}, nil
}

// Create inserts a new response snapshot into MongoDB.
func (s *MongoDBStore) Create(ctx context.Context, tenantID string, response *StoredResponse) error {
	if response == nil || response.Response == nil || response.Response.ID == "" {
		return fmt.Errorf("response id is required")
	}

	c, err := cloneResponse(response)
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
			return fmt.Errorf("response already exists: %s", c.Response.ID)
		}
		return fmt.Errorf("create response: %w", err)
	}
	return nil
}

// Get retrieves a response snapshot by tenant and id.
func (s *MongoDBStore) Get(ctx context.Context, tenantID, id string) (*StoredResponse, error) {
	result := s.coll.FindOne(ctx, tenantFilter(tenantID, id))
	if err := result.Err(); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get response: %w", err)
	}

	doc, err := result.Raw()
	if err != nil {
		return nil, fmt.Errorf("get response raw: %w", err)
	}
	return s.fromBSON(doc)
}

// Update replaces an existing response snapshot. It uses $set to replace
// all top-level fields.
func (s *MongoDBStore) Update(ctx context.Context, tenantID string, response *StoredResponse) error {
	if response == nil || response.Response == nil || response.Response.ID == "" {
		return fmt.Errorf("response id is required")
	}

	c, err := cloneResponse(response)
	if err != nil {
		return err
	}
	c.TenantID = tenantID

	updateDoc, err := s.toBSON(c)
	if err != nil {
		return err
	}

	result, err := s.coll.UpdateOne(ctx, tenantFilter(tenantID, c.Response.ID), bson.D{
		{Key: "$set", Value: updateDoc},
	})
	if err != nil {
		return fmt.Errorf("update response: %w", err)
	}
	if result.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a response snapshot by tenant and id.
func (s *MongoDBStore) Delete(ctx context.Context, tenantID, id string) error {
	result, err := s.coll.DeleteOne(ctx, tenantFilter(tenantID, id))
	if err != nil {
		return fmt.Errorf("delete response: %w", err)
	}
	if result.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

// Close is a no-op; DB lifecycle is managed by the shared storage layer.
func (s *MongoDBStore) Close() error {
	return nil
}
