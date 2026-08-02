package tenants

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"smartrouter/internal/storage"
)

// NewWithSharedStorage creates a tenant Store backed by the given shared
// storage connection. SQLite, PostgreSQL, and MongoDB backends are all
// supported.
func NewWithSharedStorage(_ context.Context, shared storage.Storage) (Store, error) {
	if shared == nil {
		return nil, fmt.Errorf("shared storage is required")
	}
	return storage.ResolveBackend[Store](
		shared,
		func(db *sql.DB) (Store, error) { return NewSQLiteStore(db) },
		func(pool *pgxpool.Pool) (Store, error) { return NewPostgreSQLStore(pool) },
		func(db *mongo.Database) (Store, error) { return NewMongoDBStore(db) },
	)
}
