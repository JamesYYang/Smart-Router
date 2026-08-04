package conversationstore

import (
	"context"
	"database/sql"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"smartrouter/internal/storage"
)

// NewStore creates the store matching the shared storage backend. Callers
// must hold a shared storage connection (see internal/app); the store itself
// does not own the backend lifecycle.
func NewStore(ctx context.Context, store storage.Storage) (Store, error) {
	return storage.ResolveBackend[Store](
		store,
		func(db *sql.DB) (Store, error) { return NewSQLiteStore(db) },
		func(pool *pgxpool.Pool) (Store, error) { return NewPostgreSQLStore(ctx, pool) },
		func(db *mongo.Database) (Store, error) { return NewMongoDBStore(ctx, db) },
	)
}
