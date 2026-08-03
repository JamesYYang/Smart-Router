package workflows

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	_ "modernc.org/sqlite"

	"smartrouter/internal/core"
)

func newTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = db.Close()
	})
	store, err := NewSQLiteStore(db)
	require.NoError(t, err)
	return store
}

func TestService_CreateForTenant_Isolation(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, NewCompiler(nil))
	require.NoError(t, err)

	verA, err := svc.CreateForTenant(context.Background(), "tenant-a", CreateInput{Name: "wf-a", Activate: true})
	require.NoError(t, err)
	verB, err := svc.CreateForTenant(context.Background(), "tenant-b", CreateInput{Name: "wf-b", Activate: true})
	require.NoError(t, err)

	listA, err := svc.ListActiveForTenant(context.Background(), "tenant-a")
	require.NoError(t, err)
	require.Len(t, listA, 1)
	require.Equal(t, verA.ID, listA[0].ID)

	listB, err := svc.ListActiveForTenant(context.Background(), "tenant-b")
	require.NoError(t, err)
	require.Len(t, listB, 1)
	require.Equal(t, verB.ID, listB[0].ID)
}

func TestService_CreateForTenant_DoesNotTouchSharedSnapshot(t *testing.T) {
	store := newTestSQLiteStore(t)
	svc, err := NewService(store, NewCompiler(nil))
	require.NoError(t, err)
	require.NoError(t, svc.EnsureDefaultGlobal(context.Background(), CreateInput{Name: "global"}))

	_, err = svc.Match(core.WorkflowSelector{}) // 走默认全局 workflow,不应报错
	require.NoError(t, err)

	_, err = svc.CreateForTenant(context.Background(), "tenant-a", CreateInput{Name: "wf-a", Activate: true})
	require.NoError(t, err)

	_, err = svc.Match(core.WorkflowSelector{}) // 共享缓存不受影响,仍能正常匹配默认全局
	require.NoError(t, err)
}
