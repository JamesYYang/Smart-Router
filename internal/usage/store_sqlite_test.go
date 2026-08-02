package usage

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/stretchr/testify/require"
)

func openTestSQLite(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	return db
}

func TestUsageSQLite_Isolation(t *testing.T) {
	db := openTestSQLite(t)
	defer db.Close()

	s, err := NewSQLiteStore(db, 0)
	require.NoError(t, err)

	r, err := NewSQLiteReader(db)
	require.NoError(t, err)

	// Write data for tenant A
	e := &UsageEntry{
		TenantID:  "A",
		Model:     "m",
		Provider:  "p",
		Endpoint:  "/v1/chat/completions",
		RequestID: "r1",
	}
	require.NoError(t, s.WriteBatch(context.Background(), "A", []*UsageEntry{e}))

	// Tenant B should see nothing
	params := UsageQueryParams{}
	sum, err := r.GetSummary(context.Background(), "B", params)
	require.NoError(t, err)
	require.Zero(t, sum.TotalRequests, "tenant B should see none of A's data")

	// Unscoped (empty tenantID) should see everything (cross-tenant)
	sumAll, err := r.GetSummary(context.Background(), "", params)
	require.NoError(t, err)
	require.Equal(t, 1, sumAll.TotalRequests, "unscoped read should see all data")
}
