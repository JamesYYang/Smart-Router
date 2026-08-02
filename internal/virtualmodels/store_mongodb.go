package virtualmodels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoVirtualModelDocument struct {
	ID           string    `bson:"_id"`
	TenantID     string    `bson:"tenant_id"`
	Source       string    `bson:"source"`
	Targets      []Target  `bson:"targets,omitempty"`
	Strategy     string    `bson:"strategy,omitempty"`
	ProviderName string    `bson:"provider_name,omitempty"`
	Model        string    `bson:"model,omitempty"`
	UserPaths    []string  `bson:"user_paths,omitempty"`
	Description  string    `bson:"description,omitempty"`
	Enabled      bool      `bson:"enabled"`
	CreatedAt    time.Time `bson:"created_at"`
	UpdatedAt    time.Time `bson:"updated_at"`
}

// mongoVMID builds the compound _id for (tenant_id, source).
func mongoVMID(tenantID, source string) string {
	return tenantID + "|" + strings.TrimSpace(source)
}

type mongoVirtualModelFilter struct {
	ID string `bson:"_id"`
}

// MongoDBStore stores virtual models in MongoDB.
type MongoDBStore struct {
	collection *mongo.Collection
}

// NewMongoDBStore creates collection indexes if needed.
func NewMongoDBStore(database *mongo.Database) (*MongoDBStore, error) {
	if database == nil {
		return nil, fmt.Errorf("database is required")
	}
	coll := database.Collection("virtual_models")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	indexes := []mongo.IndexModel{
		{Keys: bson.D{{Key: "tenant_id", Value: 1}, {Key: "source", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "provider_name", Value: 1}}},
		{Keys: bson.D{{Key: "model", Value: 1}}},
		{Keys: bson.D{{Key: "enabled", Value: 1}}},
		{Keys: bson.D{{Key: "updated_at", Value: -1}}},
	}
	if _, err := coll.Indexes().CreateMany(ctx, indexes); err != nil {
		return nil, fmt.Errorf("create virtual_models indexes: %w", err)
	}
	return &MongoDBStore{collection: coll}, nil
}

func (s *MongoDBStore) List(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	cursor, err := s.collection.Find(ctx, bson.M{"tenant_id": tenantID}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list virtual models: %w", err)
	}
	defer cursor.Close(ctx)

	result := make([]VirtualModel, 0)
	for cursor.Next(ctx) {
		var doc mongoVirtualModelDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode virtual model: %w", err)
		}
		result = append(result, virtualModelFromMongo(doc))
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate virtual models: %w", err)
	}
	return result, nil
}

func (s *MongoDBStore) ListEffective(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	cursor, err := s.collection.Find(ctx, bson.M{
		"tenant_id": bson.M{"$in": []string{"default", tenantID}},
	}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list effective virtual models: %w", err)
	}
	defer cursor.Close(ctx)

	seen := make(map[string]VirtualModel)
	for cursor.Next(ctx) {
		var doc mongoVirtualModelDocument
		if err := cursor.Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode virtual model: %w", err)
		}
		vm := virtualModelFromMongo(doc)
		seen[vm.Source] = vm
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective virtual models: %w", err)
	}

	result := make([]VirtualModel, 0, len(seen))
	for _, vm := range seen {
		result = append(result, vm)
	}
	return result, nil
}

func (s *MongoDBStore) Get(ctx context.Context, tenantID, source string) (*VirtualModel, error) {
	var doc mongoVirtualModelDocument
	err := s.collection.FindOne(ctx, mongoVirtualModelFilter{ID: mongoVMID(tenantID, source)}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get virtual model: %w", err)
	}
	vm := virtualModelFromMongo(doc)
	return &vm, nil
}

func (s *MongoDBStore) Upsert(ctx context.Context, tenantID string, vm VirtualModel) error {
	stampUpsert(&vm)
	trimmedSource := strings.TrimSpace(vm.Source)

	// Encode targets and user_paths as BSON arrays directly.
	targetsRaw, err := encodeTargets(vm.Targets)
	if err != nil {
		return err
	}
	// Targets are already JSON-marshalled;  marshal to BSON.
	var targetsBSON []Target
	if unmarshalErr := json.Unmarshal([]byte(targetsRaw), &targetsBSON); unmarshalErr != nil {
		return fmt.Errorf("decode targets for mongo upsert: %w", unmarshalErr)
	}
	if targetsBSON == nil {
		targetsBSON = []Target{}
	}

	update := bson.M{
		"$set": bson.M{
			"tenant_id":     tenantID,
			"source":        trimmedSource,
			"targets":       targetsBSON,
			"strategy":      vm.Strategy,
			"provider_name": vm.ProviderName,
			"model":         vm.Model,
			"user_paths":    vm.UserPaths,
			"description":   vm.Description,
			"enabled":       vm.Enabled,
			"updated_at":    vm.UpdatedAt,
		},
		"$setOnInsert": bson.M{
			"created_at": vm.CreatedAt,
		},
	}
	_, err = s.collection.UpdateOne(ctx, mongoVirtualModelFilter{ID: mongoVMID(tenantID, trimmedSource)}, update, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("upsert virtual model: %w", err)
	}
	return nil
}

func (s *MongoDBStore) Delete(ctx context.Context, tenantID, source string) error {
	result, err := s.collection.DeleteOne(ctx, mongoVirtualModelFilter{ID: mongoVMID(tenantID, source)})
	if err != nil {
		return fmt.Errorf("delete virtual model: %w", err)
	}
	if result.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoDBStore) Close() error {
	return nil
}

func virtualModelFromMongo(doc mongoVirtualModelDocument) VirtualModel {
	vm := VirtualModel{
		Source:       doc.Source,
		Strategy:     doc.Strategy,
		ProviderName: doc.ProviderName,
		Model:        doc.Model,
		Description:  doc.Description,
		Enabled:      doc.Enabled,
		CreatedAt:    doc.CreatedAt.UTC(),
		UpdatedAt:    doc.UpdatedAt.UTC(),
	}
	if len(doc.Targets) > 0 {
		vm.Targets = append([]Target(nil), doc.Targets...)
	}
	if len(doc.UserPaths) > 0 {
		vm.UserPaths = append([]string(nil), doc.UserPaths...)
	}
	return vm
}
