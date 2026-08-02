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
// storage connection. Only SQLite is currently implemented; PostgreSQL and
// MongoDB backends return a clear "not implemented" error.
func NewWithSharedStorage(_ context.Context, shared storage.Storage) (Store, error) {
	if shared == nil {
		return nil, fmt.Errorf("shared storage is required")
	}
	return storage.ResolveBackend[Store](
		shared,
		func(db *sql.DB) (Store, error) { return NewSQLiteStore(db) },
		func(*pgxpool.Pool) (Store, error) {
			return nil, fmt.Errorf("tenants: postgresql storage not yet implemented")
		},
		func(*mongo.Database) (Store, error) {
			return nil, fmt.Errorf("tenants: mongodb storage not yet implemented")
		},
	)
}
