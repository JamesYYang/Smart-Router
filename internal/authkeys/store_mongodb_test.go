package authkeys

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
// Mirrors the inline pattern used in internal/tenants/store_mongodb_test.go.
func newTestMongoDB(t *testing.T, url string) *mongo.Database {
	t.Helper()
	ctx := context.Background()
	client, err := mongo.Connect(options.Client().ApplyURI(url))
	require.NoError(t, err)
	dbName := "smartrouter_authkeys_test_" +
		strings.ReplaceAll(t.Name(), "/", "_") + "_" +
		time.Now().Format("20060102150405_000000000")
	db := client.Database(dbName)
	t.Cleanup(func() {
		_ = db.Drop(ctx)
		_ = client.Disconnect(ctx)
	})
	return db
}

// newTestMongoDBStore constructs a MongoDBStore against the
// SMARTROUTER_TEST_MONGO_URL database. Skips when unset.
func newTestMongoDBStore(t *testing.T) *MongoDBStore {
	t.Helper()
	url := skipIfNoMongo(t)
	db := newTestMongoDB(t, url)
	store, err := NewMongoDBStore(db)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMongoDBStore_CreateWithTenantFields(t *testing.T) {
	store := newTestMongoDBStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	require.NoError(t, store.Create(ctx, AuthKey{
		ID:            "k-mg-tenant",
		Name:          "mg admin",
		RedactedValue: "sk_gom_...",
		SecretHash:    "hash-mg-tenant",
		Enabled:       true,
		CreatedAt:     now,
		UpdatedAt:     now,
		TenantID:      "default",
		IsTenantAdmin: true,
	}))

	list, err := store.List(ctx)
	require.NoError(t, err)
	var found *AuthKey
	for i := range list {
		if list[i].ID == "k-mg-tenant" {
			found = &list[i]
			break
		}
	}
	require.NotNil(t, found)
	require.Equal(t, "default", found.TenantID)
	require.True(t, found.IsTenantAdmin)
}
