package tenants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// MongoDBStore stores tenants in MongoDB.
type MongoDBStore struct {
	coll *mongo.Collection
}

func NewMongoDBStore(db *mongo.Database) (*MongoDBStore, error) {
	if db == nil {
		return nil, fmt.Errorf("mongo database is required")
	}
	coll := db.Collection("tenants")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "subdomain", Value: 1}}, Options: options.Index().SetUnique(true)},
	})
	if err != nil {
		return nil, fmt.Errorf("create tenants indexes: %w", err)
	}
	return &MongoDBStore{coll: coll}, nil
}

type tenantDoc struct {
	ID        string `bson:"_id"`
	Subdomain string `bson:"subdomain"`
	Name      string `bson:"name"`
	Status    string `bson:"status"`
	Plan      string `bson:"plan"`
	CreatedAt int64  `bson:"created_at"`
	UpdatedAt int64  `bson:"updated_at"`
}

func (s *MongoDBStore) Create(ctx context.Context, t Tenant) error {
	_, err := s.coll.InsertOne(ctx, tenantDoc{
		ID: t.ID, Subdomain: t.Subdomain, Name: t.Name,
		Status: string(t.Status), Plan: t.Plan,
		CreatedAt: t.CreatedAt.Unix(), UpdatedAt: t.UpdatedAt.Unix(),
	})
	if err != nil {
		return fmt.Errorf("create tenant: %w", err)
	}
	return nil
}

func (s *MongoDBStore) GetByID(ctx context.Context, id string) (Tenant, error) {
	return scanMongoTenant(s.coll.FindOne(ctx, bson.M{"_id": id}))
}

func (s *MongoDBStore) GetBySubdomain(ctx context.Context, sub string) (Tenant, error) {
	return scanMongoTenant(s.coll.FindOne(ctx, bson.M{"subdomain": sub}))
}

func (s *MongoDBStore) List(ctx context.Context) ([]Tenant, error) {
	cur, err := s.coll.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer cur.Close(ctx)
	var out []Tenant
	for cur.Next(ctx) {
		t, err := scanMongoTenant(cur)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, cur.Err()
}

func (s *MongoDBStore) UpdateStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error {
	res, err := s.coll.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{"status": string(status), "updated_at": updatedAt.Unix()}})
	if err != nil {
		return fmt.Errorf("update tenant status: %w", err)
	}
	if res.MatchedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoDBStore) Close() error { return nil }

type mongoScanner interface{ Decode(v any) error }

func scanMongoTenant(sc mongoScanner) (Tenant, error) {
	var d tenantDoc
	if err := sc.Decode(&d); err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return Tenant{}, ErrNotFound
		}
		return Tenant{}, err
	}
	return Tenant{
		ID: d.ID, Subdomain: d.Subdomain, Name: d.Name, Status: Status(d.Status), Plan: d.Plan,
		CreatedAt: time.Unix(d.CreatedAt, 0).UTC(),
		UpdatedAt: time.Unix(d.UpdatedAt, 0).UTC(),
	}, nil
}
