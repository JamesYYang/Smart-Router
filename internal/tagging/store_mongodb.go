package tagging

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoDBStore persists tagging rules in a settings collection.
type MongoDBStore struct {
	settings *mongo.Collection
}

// NewMongoDBStore creates a tagging store over the tagging_settings collection.
func NewMongoDBStore(_ context.Context, database *mongo.Database) (*MongoDBStore, error) {
	if database == nil {
		return nil, fmt.Errorf("database is required")
	}
	coll := database.Collection("tagging_settings")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "key", Value: 1}}, Options: options.Index().SetUnique(true)},
	}
	if _, err := coll.Indexes().CreateMany(ctx, indexes); err != nil {
		return nil, fmt.Errorf("create tagging_settings indexes: %w", err)
	}

	if err := migrateMongoDBTaggingSettingsTenantID(ctx, coll); err != nil {
		return nil, err
	}
	return &MongoDBStore{settings: coll}, nil
}

func migrateMongoDBTaggingSettingsTenantID(ctx context.Context, coll *mongo.Collection) error {
	_, err := coll.UpdateMany(ctx,
		bson.M{"tenant_id": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"tenant_id": "default"}},
	)
	if err != nil {
		return fmt.Errorf("migrate tagging_settings tenant_id field: %w", err)
	}
	return nil
}

func mongoRulesID(tenantID, key string) string {
	return tenantID + "|" + key
}

type mongoRulesDocument struct {
	ID        string `bson:"_id"`
	TenantID  string `bson:"tenant_id"`
	Key       string `bson:"key"`
	Value     string `bson:"value"`
	UpdatedAt int64  `bson:"updated_at"`
}

func (s *MongoDBStore) GetRules(ctx context.Context, tenantID string) ([]Rule, error) {
	var doc mongoRulesDocument
	err := s.settings.FindOne(ctx, bson.D{{Key: "_id", Value: mongoRulesID(tenantID, rulesSettingKey)}}).Decode(&doc)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get tagging rules: %w", err)
	}
	return decodeRules([]byte(doc.Value))
}

func (s *MongoDBStore) SaveRules(ctx context.Context, tenantID string, rules []Rule) error {
	value, err := encodeRules(rules)
	if err != nil {
		return err
	}
	filter := bson.D{{Key: "_id", Value: mongoRulesID(tenantID, rulesSettingKey)}}
	update := bson.M{
		"$set": bson.M{
			"tenant_id":  tenantID,
			"key":        rulesSettingKey,
			"value":      string(value),
			"updated_at": time.Now().Unix(),
		},
	}
	_, err = s.settings.UpdateOne(ctx, filter, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("save tagging rules: %w", err)
	}
	return nil
}

func (s *MongoDBStore) ListEffectiveRules(ctx context.Context, tenantID string) ([]Rule, error) {
	cursor, err := s.settings.Find(ctx, bson.M{
		"tenant_id": bson.M{"$in": []string{"default", tenantID}},
		"key":       rulesSettingKey,
	}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list effective tagging rules: %w", err)
	}
	defer cursor.Close(ctx)

	byHeader := make(map[string]Rule)
	for cursor.Next(ctx) {
		var doc struct {
			Value string `bson:"value"`
		}
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode effective tagging doc: %w", err)
		}
		rules, err := decodeRules([]byte(doc.Value))
		if err != nil {
			return nil, fmt.Errorf("decode effective tagging rules: %w", err)
		}
		for _, rule := range rules {
			byHeader[rule.Header] = rule
		}
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective tagging rules: %w", err)
	}

	merged := make([]Rule, 0, len(byHeader))
	for _, rule := range byHeader {
		merged = append(merged, rule)
	}
	return merged, nil
}

// Close is a no-op: the client is managed by the storage layer.
func (s *MongoDBStore) Close() error {
	return nil
}
