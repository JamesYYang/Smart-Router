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

	before, err := svc.Match(context.Background(), core.WorkflowSelector{}) // 走默认全局 workflow,不应报错
	require.NoError(t, err)
	require.NotNil(t, before)

	created, err := svc.CreateForTenant(context.Background(), "tenant-a", CreateInput{Name: "wf-a", Activate: true})
	require.NoError(t, err)
	require.NotEqual(t, before.VersionID, created.ID, "tenant-a 版本必须与默认全局不同,否则本断言无意义")

	after, err := svc.Match(context.Background(), core.WorkflowSelector{}) // 共享缓存不受影响,仍应命中同一个默认全局版本
	require.NoError(t, err)
	require.NotNil(t, after)
	// 若实现错误地刷新了共享快照(例如误调 Refresh 或
	// storeActivatedCompiledLocked 把 tenant-a 的全局 workflow 写入快照),
	// Match 命中的将是 tenant-a 的版本(Name 为 "wf-a"),身份断言即失败。
	require.Equal(t, before.VersionID, after.VersionID, "tenant-a 写入后共享快照被污染,Match 命中的版本发生变化")
	require.Equal(t, before.Name, after.Name, "tenant-a 写入后共享快照被污染,Match 命中的 workflow 名称发生变化")
}
