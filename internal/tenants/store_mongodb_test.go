package tenants

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func skipIfNoMongo(t *testing.T) string {
	t.Helper()
	url := os.Getenv("SMARTROUTER_TEST_MONGO_URL")
	if url == "" {
		t.Skip("set SMARTROUTER_TEST_MONGO_URL to run")
	}
	return url
}

// newTestMongoDB creates a *mongo.Database with a unique name for isolation.
// The database is dropped and the client disconnected when the test ends.
// No project-wide shared helper exists, so this mirrors the inline pattern
// used in internal/filestore/store_test.go.
func newTestMongoDB(t *testing.T, url string) *mongo.Database {
	t.Helper()
	ctx := context.Background()
	client, err := mongo.Connect(options.Client().ApplyURI(url))
	require.NoError(t, err)
	dbName := "smartrouter_tenants_test_" +
		strings.ReplaceAll(t.Name(), "/", "_") + "_" +
		time.Now().Format("20060102150405_000000000")
	db := client.Database(dbName)
	t.Cleanup(func() {
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	return db
}

func TestMongoDBStore_CRUD(t *testing.T) {
	url := skipIfNoMongo(t)
	db := newTestMongoDB(t, url)
	store, err := NewMongoDBStore(db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, Tenant{ID: "t-mg-1", Subdomain: "mgxyz", Name: "MG", Status: StatusActive, CreatedAt: now, UpdatedAt: now}))

	got, err := store.GetBySubdomain(ctx, "mgxyz")
	require.NoError(t, err)
	require.Equal(t, "t-mg-1", got.ID)
}
