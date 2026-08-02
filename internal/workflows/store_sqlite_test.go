package workflows

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func TestNewSQLiteStore_SkipsExistingScopeUserPathMigration(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE workflow_versions (
			id TEXT PRIMARY KEY,
			scope_provider TEXT,
			scope_model TEXT,
			scope_user_path TEXT,
			scope_key TEXT NOT NULL,
			version INTEGER NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			workflow_payload JSON NOT NULL,
			workflow_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create workflow_versions table: %v", err)
	}

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	if store == nil {
		t.Fatal("NewSQLiteStore() = nil, want store")
	}
}

func TestNewSQLiteStore_AddsMissingScopeUserPathColumn(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE workflow_versions (
			id TEXT PRIMARY KEY,
			scope_provider TEXT,
			scope_model TEXT,
			scope_key TEXT NOT NULL,
			version INTEGER NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			name TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			workflow_payload JSON NOT NULL,
			workflow_hash TEXT NOT NULL,
			created_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create workflow_versions table: %v", err)
	}

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	if store == nil {
		t.Fatal("NewSQLiteStore() = nil, want store")
	}

	rows, err := db.Query(`PRAGMA table_info('workflow_versions')`)
	if err != nil {
		t.Fatalf("PRAGMA table_info() error = %v", err)
	}
	defer rows.Close()

	hasScopeUserPathColumn := false
	for rows.Next() {
		var cid int
		var name string
		var columnType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("rows.Scan() error = %v", err)
		}
		if name == "scope_user_path" {
			hasScopeUserPathColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() = %v", err)
	}
	if !hasScopeUserPathColumn {
		t.Fatal("scope_user_path column missing after initialization")
	}
}

func TestSQLiteStore_TenantIsolation(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}

	// Seed default tenant with a managed default global.
	_, err = store.EnsureManagedDefaultGlobal(context.Background(), "default", CreateInput{
		Activate: true, Name: "default-global", Description: "Bootstrapped from runtime configuration",
		Payload: Payload{SchemaVersion: 1, Features: FeatureFlags{Cache: true, Audit: true, Usage: true, Guardrails: false}},
	}, "hash-default")
	if err != nil {
		t.Fatalf("EnsureManagedDefaultGlobal(default) error = %v", err)
	}

	// Create a tenant-specific workflow for "A".
	_, err = store.Create(context.Background(), "A", CreateInput{
		Scope: Scope{Provider: "openai"}, Activate: true,
		Name:    "openai-workflow",
		Payload: Payload{SchemaVersion: 1, Features: FeatureFlags{Cache: false, Audit: true, Usage: true, Guardrails: false}},
	})
	if err != nil {
		t.Fatalf("Create(A/openai) error = %v", err)
	}

	// ListActive for default should only show default's global.
	defaultActive, err := store.ListActive(context.Background(), "default")
	if err != nil {
		t.Fatalf("ListActive(default) error = %v", err)
	}
	if len(defaultActive) != 1 {
		t.Fatalf("len(defaultActive) = %d, want 1", len(defaultActive))
	}

	// ListActive for A should only show A's active.
	aActive, err := store.ListActive(context.Background(), "A")
	if err != nil {
		t.Fatalf("ListActive(A) error = %v", err)
	}
	if len(aActive) != 1 {
		t.Fatalf("len(aActive) = %d, want 1", len(aActive))
	}
	if aActive[0].Name != "openai-workflow" {
		t.Fatalf("aActive[0].Name = %q, want openai-workflow", aActive[0].Name)
	}

	// ListActive for B should have no results.
	bActive, err := store.ListActive(context.Background(), "B")
	if err != nil {
		t.Fatalf("ListActive(B) error = %v", err)
	}
	if len(bActive) != 0 {
		t.Fatalf("len(bActive) = %d, want 0", len(bActive))
	}
}

func TestSQLiteStore_ListEffectiveMerge(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()

	store, err := NewSQLiteStore(db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}

	// Seed default tenant: global + path.
	_, err = store.EnsureManagedDefaultGlobal(context.Background(), "default", CreateInput{
		Activate: true, Name: "default-global", Description: "Bootstrapped from runtime configuration",
		Payload: Payload{SchemaVersion: 1, Features: FeatureFlags{Cache: true, Audit: true, Usage: true, Guardrails: false}},
	}, "hash-global")
	if err != nil {
		t.Fatalf("EnsureManagedDefaultGlobal default error = %v", err)
	}
	_, err = store.Create(context.Background(), "default", CreateInput{
		Scope: Scope{UserPath: "/team"}, Activate: true,
		Name:    "default-path",
		Payload: Payload{SchemaVersion: 1, Features: FeatureFlags{Cache: true, Audit: true, Usage: true, Guardrails: false}},
	})
	if err != nil {
		t.Fatalf("Create(default/path) error = %v", err)
	}

	// Seed tenant A: override the global (same scope_key).
	_, err = store.Create(context.Background(), "A", CreateInput{
		Scope: Scope{}, Activate: true,
		Name:    "A-global",
		Payload: Payload{SchemaVersion: 1, Features: FeatureFlags{Cache: false, Audit: false, Usage: false, Guardrails: false}},
	})
	if err != nil {
		t.Fatalf("Create(A/global) error = %v", err)
	}
	// Seed tenant A: a-specific workflow.
	_, err = store.Create(context.Background(), "A", CreateInput{
		Scope: Scope{Provider: "openai"}, Activate: true,
		Name:    "A-openai",
		Payload: Payload{SchemaVersion: 1, Features: FeatureFlags{Cache: false, Audit: true, Usage: true, Guardrails: false}},
	})
	if err != nil {
		t.Fatalf("Create(A/openai) error = %v", err)
	}

	// ListEffective for A: should include A's global override, A's openai, and default's path.
	effective, err := store.ListEffective(context.Background(), "A")
	if err != nil {
		t.Fatalf("ListEffective(A) error = %v", err)
	}

	byScope := make(map[string]Version)
	for _, v := range effective {
		byScope[v.ScopeKey] = v
	}

	// A's global should override default's global.
	if global, ok := byScope["global"]; !ok {
		t.Fatal("effective missing 'global' scope")
	} else if global.Name != "A-global" {
		t.Fatalf("global.Name = %q, want A-global", global.Name)
	}

	// A's openai should be present.
	if openai, ok := byScope["provider:openai"]; !ok {
		t.Fatal("effective missing 'provider:openai' scope")
	} else if openai.Name != "A-openai" {
		t.Fatalf("openai.Name = %q, want A-openai", openai.Name)
	}

	// Default's path should be inherited by A.
	if path, ok := byScope["path:/team"]; !ok {
		t.Fatal("effective missing 'path:/team' scope")
	} else if path.Name != "default-path" {
		t.Fatalf("path.Name = %q, want default-path", path.Name)
	}
}
