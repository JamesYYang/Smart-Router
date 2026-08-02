package guardrails

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"smartrouter/internal/core"
)

type mongoDefinitionDocument struct {
	Name        string    `bson:"name"`
	TenantID    string    `bson:"tenant_id"`
	Type        string    `bson:"type"`
	Description string    `bson:"description,omitempty"`
	UserPath    string    `bson:"user_path,omitempty"`
	Config      bson.M    `bson:"config"`
	CreatedAt   time.Time `bson:"created_at"`
	UpdatedAt   time.Time `bson:"updated_at"`
}

func mongoGuardrailID(tenantID, name string) string {
	return tenantID + "|" + strings.TrimSpace(name)
}

type mongoDefinitionIDFilter struct {
	ID string `bson:"_id"`
}

// MongoDBStore stores guardrail definitions in MongoDB.
type MongoDBStore struct {
	collection *mongo.Collection
}

func migrateMongoDBGuardrailTenantID(ctx context.Context, coll *mongo.Collection) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if _, err := coll.UpdateMany(ctx,
		bson.M{"tenant_id": bson.M{"$exists": false}},
		bson.M{"$set": bson.M{"tenant_id": "default"}},
	); err != nil {
		return fmt.Errorf("migrate guardrail_definitions tenant_id field: %w", err)
	}

	// Re-key documents that use the old _id (name only) by inserting under the new _id format
	// and deleting the old one. This is best-effort and safe when run multiple times.
	return nil
}

func fixMongoGuardrailID(ctx context.Context, coll *mongo.Collection) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cursor, err := coll.Find(ctx, bson.M{}, options.Find().SetBatchSize(int32(500)))
	if err != nil {
		return fmt.Errorf("find guardrails for rekey: %w", err)
	}
	defer cursor.Close(ctx)

	var fixOps []mongo.WriteModel
	for cursor.Next(ctx) {
		var doc mongoDefinitionDocument
		if err := cursor.Decode(&doc); err != nil {
			return fmt.Errorf("decode guardrail for rekey: %w", err)
		}
		expected := mongoGuardrailID(doc.TenantID, doc.Name)
		if doc.Name != "" && expected == doc.Name {
			continue
		}
		// Use _id field from raw bson to get the actual stored _id
		var raw bson.Raw
		if err := cursor.Decode(&raw); err != nil {
			continue
		}
		oldID := raw.Lookup("_id").StringValue()
		if oldID == expected || oldID == "" {
			continue
		}
		doc.Name = expected
		fixOps = append(fixOps, mongo.NewDeleteOneModel().SetFilter(bson.M{"_id": oldID}))
		// Insert under new _id
		fixOps = append(fixOps, mongo.NewInsertOneModel().SetDocument(bson.M{
			"_id":          expected,
			"tenant_id":    doc.TenantID,
			"name":         doc.Name,
			"type":         doc.Type,
			"description":  doc.Description,
			"user_path":    doc.UserPath,
			"config":       doc.Config,
			"created_at":   doc.CreatedAt,
			"updated_at":   doc.UpdatedAt,
		}))
	}
	if err := cursor.Err(); err != nil {
		return fmt.Errorf("iterate guardrails for rekey: %w", err)
	}

	if len(fixOps) > 0 {
		if _, err := coll.BulkWrite(ctx, fixOps, options.BulkWrite().SetOrdered(false)); err != nil {
			return fmt.Errorf("rekey guardrail documents: %w", err)
		}
	}
	return nil
}

// NewMongoDBStore creates collection indexes if needed.
func NewMongoDBStore(ctx context.Context, database *mongo.Database) (*MongoDBStore, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if database == nil {
		return nil, fmt.Errorf("database is required")
	}
	coll := database.Collection("guardrail_definitions")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if count, err := coll.EstimatedDocumentCount(ctx); err == nil && count > 0 {
		// Migration: stamp tenant_id on existing docs
		_ = migrateMongoDBGuardrailTenantID(ctx, coll)
		// Re-key old _id docs to new compound format
		_ = fixMongoGuardrailID(ctx, coll)
	}

	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "type", Value: 1}}},
		{Keys: bson.D{{Key: "updated_at", Value: -1}}},
	}
	if _, err := coll.Indexes().CreateMany(ctx, indexes); err != nil {
		return nil, fmt.Errorf("create guardrail indexes: %w", err)
	}
	return &MongoDBStore{collection: coll}, nil
}

func (s *MongoDBStore) List(ctx context.Context, tenantID string) ([]Definition, error) {
	cursor, err := s.collection.Find(ctx, bson.M{"tenant_id": tenantID}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list guardrails: %w", err)
	}
	defer cursor.Close(ctx)

	result := make([]Definition, 0)
	for cursor.Next(ctx) {
		var doc mongoDefinitionDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode guardrail: %w", err)
		}
		definition, err := definitionFromMongo(doc)
		if err != nil {
			return nil, err
		}
		result = append(result, definition)
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate guardrails: %w", err)
	}
	return result, nil
}

func (s *MongoDBStore) ListEffective(ctx context.Context, tenantID string) ([]Definition, error) {
	cursor, err := s.collection.Find(ctx,
		bson.M{"tenant_id": bson.M{"$in": []string{"default", tenantID}}},
		options.Find().SetSort(bson.D{{Key: "name", Value: 1}, {Key: "tenant_id", Value: 1}}),
	)
	if err != nil {
		return nil, fmt.Errorf("list effective guardrails: %w", err)
	}
	defer cursor.Close(ctx)

	seen := make(map[string]Definition)
	for cursor.Next(ctx) {
		var doc mongoDefinitionDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode guardrail: %w", err)
		}
		definition, err := definitionFromMongo(doc)
		if err != nil {
			return nil, err
		}
		seen[definition.Name] = definition
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective guardrails: %w", err)
	}

	result := make([]Definition, 0, len(seen))
	for _, definition := range seen {
		result = append(result, definition)
	}
	return result, nil
}

func (s *MongoDBStore) Get(ctx context.Context, tenantID, name string) (*Definition, error) {
	var doc mongoDefinitionDocument
	err := s.collection.FindOne(ctx, bson.M{"tenant_id": tenantID, "name": normalizeDefinitionName(name)}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get guardrail: %w", err)
	}
	definition, err := definitionFromMongo(doc)
	if err != nil {
		return nil, err
	}
	return &definition, nil
}

func (s *MongoDBStore) Upsert(ctx context.Context, tenantID string, definition Definition) error {
	definition, err := normalizeDefinition(definition)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if definition.CreatedAt.IsZero() {
		definition.CreatedAt = now
	}
	definition.UpdatedAt = now

	configDoc, err := mongoConfigFromRaw(definition.Config)
	if err != nil {
		return fmt.Errorf("upsert guardrail: %w", err)
	}

	update := bson.M{
		"$set": bson.M{
			"tenant_id":   tenantID,
			"name":        definition.Name,
			"type":        definition.Type,
			"description": definition.Description,
			"user_path":   definition.UserPath,
			"config":      configDoc,
			"updated_at":  definition.UpdatedAt,
		},
		"$setOnInsert": bson.M{
			"created_at": definition.CreatedAt,
		},
	}
	_, err = s.collection.UpdateOne(ctx, bson.M{"tenant_id": tenantID, "name": definition.Name}, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("upsert guardrail: %w", err)
	}
	return nil
}

func (s *MongoDBStore) UpsertMany(ctx context.Context, tenantID string, definitions []Definition) error {
	if len(definitions) == 0 {
		return nil
	}

	now := time.Now().UTC()
	models := make([]mongo.WriteModel, 0, len(definitions))
	for _, definition := range definitions {
		normalized, err := normalizeDefinition(definition)
		if err != nil {
			return err
		}
		if normalized.CreatedAt.IsZero() {
			normalized.CreatedAt = now
		}
		normalized.UpdatedAt = now

		configDoc, err := mongoConfigFromRaw(normalized.Config)
		if err != nil {
			return fmt.Errorf("upsert guardrail %q: %w", normalized.Name, err)
		}

		update := bson.M{
			"$set": bson.M{
				"tenant_id":   tenantID,
				"name":        normalized.Name,
				"type":        normalized.Type,
				"description": normalized.Description,
				"user_path":   normalized.UserPath,
				"config":      configDoc,
				"updated_at":  normalized.UpdatedAt,
			},
			"$setOnInsert": bson.M{
				"created_at": normalized.CreatedAt,
			},
		}
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"tenant_id": tenantID, "name": normalized.Name}).
			SetUpdate(update).
			SetUpsert(true))
	}

	session, err := s.collection.Database().Client().StartSession()
	if err != nil {
		return fmt.Errorf("start guardrail upsert session: %w", err)
	}
	defer session.EndSession(ctx)

	_, err = session.WithTransaction(ctx, func(sessionCtx context.Context) (any, error) {
		if _, err := s.collection.BulkWrite(sessionCtx, models, options.BulkWrite().SetOrdered(true)); err != nil {
			return nil, fmt.Errorf("bulk upsert guardrails: %w", err)
		}
		return nil, nil
	})
	if err != nil {
		return fmt.Errorf("upsert guardrails: %w", err)
	}
	return nil
}

func (s *MongoDBStore) Delete(ctx context.Context, tenantID, name string) error {
	result, err := s.collection.DeleteOne(ctx, bson.M{"tenant_id": tenantID, "name": normalizeDefinitionName(name)})
	if err != nil {
		return fmt.Errorf("delete guardrail: %w", err)
	}
	if result.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoDBStore) Close() error {
	return nil
}

func mongoConfigFromRaw(raw json.RawMessage) (bson.M, error) {
	trimmed := bytes.TrimSpace(raw)
	if core.IsJSONNull(trimmed) {
		return bson.M{}, nil
	}
	var doc bson.M
	if err := json.Unmarshal(trimmed, &doc); err != nil {
		return nil, fmt.Errorf("decode guardrail config: %w", err)
	}
	if doc == nil {
		return bson.M{}, nil
	}
	return doc, nil
}

func definitionFromMongo(doc mongoDefinitionDocument) (Definition, error) {
	config := doc.Config
	if config == nil {
		config = bson.M{}
	}
	raw, err := json.Marshal(config)
	if err != nil {
		return Definition{}, fmt.Errorf("encode guardrail config %q: %w", doc.Name, err)
	}
	return Definition{
		Name:        doc.Name,
		Type:        doc.Type,
		Description: doc.Description,
		UserPath:    doc.UserPath,
		Config:      raw,
		CreatedAt:   doc.CreatedAt.UTC(),
		UpdatedAt:   doc.UpdatedAt.UTC(),
	}, nil
}
