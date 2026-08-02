package pricingoverrides

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoOverrideDocument struct {
	TenantID     string    `bson:"tenant_id"`
	ID           string    `bson:"_id"`
	ProviderName string    `bson:"provider_name,omitempty"`
	Model        string    `bson:"model,omitempty"`
	Pricing      Pricing   `bson:"pricing"`
	CreatedAt    time.Time `bson:"created_at"`
	UpdatedAt    time.Time `bson:"updated_at"`
}

func mongoOverrideID(tenantID, selector string) string {
	return tenantID + "|" + strings.TrimSpace(selector)
}

type mongoOverrideIDFilter struct {
	ID string `bson:"_id"`
}

// MongoDBStore stores model pricing overrides in MongoDB.
type MongoDBStore struct {
	collection *mongo.Collection
}

// NewMongoDBStore creates collection indexes if needed.
func NewMongoDBStore(database *mongo.Database) (*MongoDBStore, error) {
	if database == nil {
		return nil, fmt.Errorf("database is required")
	}
	coll := database.Collection("model_pricing_overrides")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}}},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "provider_name", Value: 1}}},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "model", Value: 1}}},
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "updated_at", Value: -1}}},
	}
	if _, err := coll.Indexes().CreateMany(ctx, indexes); err != nil {
		return nil, fmt.Errorf("create model_pricing_overrides indexes: %w", err)
	}

	if err := migrateMongoDBPricingOverridesTenantID(ctx, coll); err != nil {
		return nil, err
	}
	return &MongoDBStore{collection: coll}, nil
}

func migrateMongoDBPricingOverridesTenantID(ctx context.Context, coll *mongo.Collection) error {
	_, err := coll.UpdateMany(ctx,
		bson.M{"tenant_id": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"tenant_id": "default"}},
	)
	if err != nil {
		return fmt.Errorf("migrate model_pricing_overrides tenant_id field: %w", err)
	}
	return nil
}

func (s *MongoDBStore) List(ctx context.Context, tenantID string) ([]Override, error) {
	cursor, err := s.collection.Find(ctx, bson.M{"tenant_id": tenantID}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list model pricing overrides: %w", err)
	}
	defer cursor.Close(ctx)

	result := make([]Override, 0)
	for cursor.Next(ctx) {
		var doc mongoOverrideDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode model pricing override: %w", err)
		}
		result = append(result, overrideFromMongo(doc))
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate model pricing overrides: %w", err)
	}
	return result, nil
}

func (s *MongoDBStore) ListEffective(ctx context.Context, tenantID string) ([]Override, error) {
	cursor, err := s.collection.Find(ctx, bson.M{
		"tenant_id": bson.M{"$in": []string{"default", tenantID}},
	}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list effective model pricing overrides: %w", err)
	}
	defer cursor.Close(ctx)

	seen := make(map[string]Override)
	for cursor.Next(ctx) {
		var doc mongoOverrideDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode effective model pricing override: %w", err)
		}
		override := overrideFromMongo(doc)
		seen[override.Selector] = override
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective model pricing overrides: %w", err)
	}

	result := make([]Override, 0, len(seen))
	for _, override := range seen {
		result = append(result, override)
	}
	return result, nil
}

func (s *MongoDBStore) Upsert(ctx context.Context, tenantID string, override Override) error {
	override, err := normalizeStoredOverride(override)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	if override.CreatedAt.IsZero() {
		override.CreatedAt = now
	}
	override.UpdatedAt = now

	update := bson.M{
		"$set": bson.M{
			"tenant_id":     tenantID,
			"provider_name": override.ProviderName,
			"model":         override.Model,
			"pricing":       override.Pricing,
			"updated_at":    override.UpdatedAt,
		},
		"$setOnInsert": bson.M{
			"created_at": override.CreatedAt,
		},
	}
	_, err = s.collection.UpdateOne(ctx, mongoOverrideIDFilter{ID: mongoOverrideID(tenantID, override.Selector)}, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("upsert model pricing override: %w", err)
	}
	return nil
}

func (s *MongoDBStore) Delete(ctx context.Context, tenantID, selector string) error {
	result, err := s.collection.DeleteOne(ctx, mongoOverrideIDFilter{ID: mongoOverrideID(tenantID, selector)})
	if err != nil {
		return fmt.Errorf("delete model pricing override: %w", err)
	}
	if result.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoDBStore) Close() error {
	return nil
}

func overrideFromMongo(doc mongoOverrideDocument) Override {
	return Override{
		Selector:     doc.ID,
		ProviderName: doc.ProviderName,
		Model:        doc.Model,
		Pricing:      clonePricing(doc.Pricing),
		CreatedAt:    doc.CreatedAt.UTC(),
		UpdatedAt:    doc.UpdatedAt.UTC(),
	}
}
