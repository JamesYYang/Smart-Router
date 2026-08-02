package failover

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoRuleDocument struct {
	ID             string    `bson:"_id"`
	TenantID       string    `bson:"tenant_id"`
	Source         string    `bson:"source"`
	Targets        []string  `bson:"fallback_models"`
	Enabled        bool      `bson:"enabled"`
	ManagedSource  string    `bson:"managed_source"`
	CreatedAt      time.Time `bson:"created_at"`
	UpdatedAt      time.Time `bson:"updated_at"`
}

// mongoRuleID builds the compound _id for (tenant_id, primary_model).
func mongoRuleID(tenantID, source string) string {
	return tenantID + "|" + strings.TrimSpace(source)
}

type mongoRuleFilter struct {
	ID string `bson:"_id"`
}

type MongoDBStore struct {
	collection *mongo.Collection
}

func NewMongoDBStore(database *mongo.Database) (*MongoDBStore, error) {
	if database == nil {
		return nil, fmt.Errorf("database is required")
	}
	coll := database.Collection("failover_rules")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "source", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "enabled", Value: 1}}},
		{Keys: bson.D{{Key: "updated_at", Value: -1}}},
	}
	if _, err := coll.Indexes().CreateMany(ctx, indexes); err != nil {
		return nil, fmt.Errorf("create failover_rules indexes: %w", err)
	}
	if err := migrateMongoDBFailoverRulesTenantID(ctx, coll); err != nil {
		return nil, err
	}
	if err := migrateMongoDBFailoverRules(ctx, coll); err != nil {
		return nil, err
	}
	return &MongoDBStore{collection: coll}, nil
}

// migrateMongoDBFailoverRulesTenantID stamps tenant_id='default' on documents
// that were created before P3 multi-tenant work and lack the field.
func migrateMongoDBFailoverRulesTenantID(ctx context.Context, coll *mongo.Collection) error {
	_, err := coll.UpdateMany(ctx,
		bson.M{"tenant_id": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"tenant_id": "default"}},
	)
	if err != nil {
		return fmt.Errorf("migrate failover_rules tenant_id field: %w", err)
	}
	return nil
}

func migrateMongoDBFailoverRules(ctx context.Context, coll *mongo.Collection) error {
	if _, err := coll.UpdateMany(ctx,
		bson.M{"fallback_models": bson.M{"$exists": false}, "targets": bson.M{"$exists": true}},
		mongo.Pipeline{
			bson.D{{Key: "$set", Value: bson.D{{Key: "fallback_models", Value: "$targets"}}}},
		},
	); err != nil {
		return fmt.Errorf("migrate failover_rules targets field: %w", err)
	}
	if _, err := coll.UpdateMany(ctx,
		bson.M{"$or": bson.A{
			bson.M{"targets": bson.M{"$exists": true}},
			bson.M{"description": bson.M{"$exists": true}},
		}},
		bson.M{"$unset": bson.M{"targets": "", "description": ""}},
	); err != nil {
		return fmt.Errorf("remove legacy failover_rules fields: %w", err)
	}
	return nil
}

func (s *MongoDBStore) List(ctx context.Context, tenantID string) ([]Rule, error) {
	cursor, err := s.collection.Find(ctx, bson.M{"tenant_id": tenantID}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list failover mappings: %w", err)
	}
	defer cursor.Close(ctx)
	result := make([]Rule, 0)
	for cursor.Next(ctx) {
		var doc mongoRuleDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode failover mapping: %w", err)
		}
		result = append(result, ruleFromMongo(doc))
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate failover mappings: %w", err)
	}
	return result, nil
}

func (s *MongoDBStore) ListEffective(ctx context.Context, tenantID string) ([]Rule, error) {
	cursor, err := s.collection.Find(ctx, bson.M{
		"tenant_id": bson.M{"$in": []string{"default", tenantID}},
	}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list effective failover mappings: %w", err)
	}
	defer cursor.Close(ctx)

	seen := make(map[string]Rule)
	for cursor.Next(ctx) {
		var doc mongoRuleDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode failover mapping: %w", err)
		}
		rule := ruleFromMongo(doc)
		seen[rule.Source] = rule
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective failover mappings: %w", err)
	}

	result := make([]Rule, 0, len(seen))
	for _, rule := range seen {
		result = append(result, rule)
	}
	return result, nil
}

func (s *MongoDBStore) Get(ctx context.Context, tenantID, source string) (*Rule, error) {
	var doc mongoRuleDocument
	err := s.collection.FindOne(ctx, mongoRuleFilter{ID: mongoRuleID(tenantID, source)}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get failover mapping: %w", err)
	}
	rule := ruleFromMongo(doc)
	return &rule, nil
}

func (s *MongoDBStore) Upsert(ctx context.Context, tenantID string, rule Rule) error {
	stampUpsert(&rule)
	trimmedSource := strings.TrimSpace(rule.Source)

	update := bson.M{
		"$set": bson.M{
			"tenant_id":       tenantID,
			"source":          trimmedSource,
			"fallback_models": rule.Targets,
			"enabled":         rule.Enabled,
			"managed_source":  rule.ManagedSource,
			"updated_at":      rule.UpdatedAt,
		},
		"$setOnInsert": bson.M{
			"created_at": rule.CreatedAt,
		},
	}
	_, err := s.collection.UpdateOne(ctx, mongoRuleFilter{ID: mongoRuleID(tenantID, trimmedSource)}, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("upsert failover mapping: %w", err)
	}
	return nil
}

func (s *MongoDBStore) Delete(ctx context.Context, tenantID, source string) error {
	result, err := s.collection.DeleteOne(ctx, mongoRuleFilter{ID: mongoRuleID(tenantID, source)})
	if err != nil {
		return fmt.Errorf("delete failover mapping: %w", err)
	}
	if result.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoDBStore) DeleteAll(ctx context.Context, tenantID string) error {
	if _, err := s.collection.DeleteMany(ctx, bson.M{"tenant_id": tenantID}); err != nil {
		return fmt.Errorf("delete failover mappings: %w", err)
	}
	return nil
}

func (s *MongoDBStore) Close() error { return nil }

func ruleFromMongo(doc mongoRuleDocument) Rule {
	targets := doc.Targets
	if len(targets) == 0 {
		targets = nil
	}
	return Rule{
		Source:        strings.TrimSpace(doc.Source),
		Targets:       append([]string(nil), targets...),
		Enabled:       doc.Enabled,
		ManagedSource: doc.ManagedSource,
		CreatedAt:     doc.CreatedAt.UTC(),
		UpdatedAt:     doc.UpdatedAt.UTC(),
	}
}
