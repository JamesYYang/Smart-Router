package guardrails

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	_ "modernc.org/sqlite"
)

func TestNewSQLiteStore_AddsMissingUserPathColumn(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	_, err = db.Exec(`
		CREATE TABLE guardrail_definitions (
			name TEXT PRIMARY KEY,
			type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			config JSON NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("create guardrail_definitions table: %v", err)
	}

	store, err := NewSQLiteStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}
	if store == nil {
		t.Fatal("NewSQLiteStore() = nil, want store")
	}

	rows, err := db.Query(`PRAGMA table_info('guardrail_definitions')`)
	if err != nil {
		t.Fatalf("PRAGMA table_info() error = %v", err)
	}
	defer rows.Close()

	hasUserPathColumn := false
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
		if name == "user_path" {
			hasUserPathColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() = %v", err)
	}
	if !hasUserPathColumn {
		t.Fatal("user_path column missing after initialization")
	}
}

func TestSQLiteStore_UpsertAndListRoundTripsUserPath(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	store, err := NewSQLiteStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}

	err = store.Upsert(context.Background(), "default", Definition{
		Name:        "policy-system",
		Type:        "system_prompt",
		Description: "Default policy",
		UserPath:    "/team/alpha",
		Config:      rawConfig(t, map[string]any{"mode": "inject", "content": "be careful"}),
	})
	if err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	definitions, err := store.List(context.Background(), "default")
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(definitions) != 1 {
		t.Fatalf("len(definitions) = %d, want 1", len(definitions))
	}
	if definitions[0].UserPath != "/team/alpha" {
		t.Fatalf("definitions[0].UserPath = %q, want /team/alpha", definitions[0].UserPath)
	}
}

func TestIsSQLiteDuplicateColumnError_RequiresColumnContext(t *testing.T) {
	t.Parallel()

	if !isSQLiteDuplicateColumnError(errors.New("duplicate column name: user_path")) {
		t.Fatal("isSQLiteDuplicateColumnError() = false, want true for duplicate column")
	}
	if !isSQLiteDuplicateColumnError(errors.New("column user_path already exists")) {
		t.Fatal("isSQLiteDuplicateColumnError() = false, want true for existing column")
	}
	if isSQLiteDuplicateColumnError(errors.New("table guardrail_definitions already exists")) {
		t.Fatal("isSQLiteDuplicateColumnError() = true, want false for non-column already exists")
	}
}

func TestSQLiteStore_TenantIsolation(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	store, err := NewSQLiteStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}

	// Seed default tenant.
	err = store.Upsert(context.Background(), "default", Definition{
		Name: "policy", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "default-content"}),
	})
	if err != nil {
		t.Fatalf("Upsert(default) error = %v", err)
	}

	// Seed tenant A.
	err = store.Upsert(context.Background(), "A", Definition{
		Name: "policy", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "override", "content": "A-content"}),
	})
	if err != nil {
		t.Fatalf("Upsert(A) error = %v", err)
	}

	// List for default should only show default entries.
	defaultDefs, err := store.List(context.Background(), "default")
	if err != nil {
		t.Fatalf("List(default) error = %v", err)
	}
	if len(defaultDefs) != 1 {
		t.Fatalf("len(defaultDefs) = %d, want 1", len(defaultDefs))
	}

	// List for A should only show A entries.
	aDefs, err := store.List(context.Background(), "A")
	if err != nil {
		t.Fatalf("List(A) error = %v", err)
	}
	if len(aDefs) != 1 {
		t.Fatalf("len(aDefs) = %d, want 1", len(aDefs))
	}
}

func TestSQLiteStore_ListEffectiveMerge(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	store, err := NewSQLiteStore(context.Background(), db)
	if err != nil {
		t.Fatalf("NewSQLiteStore() error = %v", err)
	}

	// Seed default tenant with "g" and "shared".
	err = store.Upsert(context.Background(), "default", Definition{
		Name: "g", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "default-g"}),
	})
	if err != nil {
		t.Fatalf("Upsert(default/g) error = %v", err)
	}
	err = store.Upsert(context.Background(), "default", Definition{
		Name: "shared", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "default-shared"}),
	})
	if err != nil {
		t.Fatalf("Upsert(default/shared) error = %v", err)
	}

	// Seed tenant A with "g" (override) and "a-only".
	err = store.Upsert(context.Background(), "A", Definition{
		Name: "g", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "override", "content": "A-g"}),
	})
	if err != nil {
		t.Fatalf("Upsert(A/g) error = %v", err)
	}
	err = store.Upsert(context.Background(), "A", Definition{
		Name: "a-only", Type: "system_prompt",
		Config: rawConfig(t, map[string]any{"mode": "inject", "content": "A-only"}),
	})
	if err != nil {
		t.Fatalf("Upsert(A/a-only) error = %v", err)
	}

	// ListEffective for A: should get A's "g", default's "shared", and A's "a-only".
	effective, err := store.ListEffective(context.Background(), "A")
	if err != nil {
		t.Fatalf("ListEffective(A) error = %v", err)
	}

	nameMap := make(map[string]Definition, len(effective))
	for _, def := range effective {
		nameMap[def.Name] = def
	}

	if len(nameMap) != 3 {
		t.Fatalf("len(effective) = %d, want 3 (g, shared, a-only)", len(effective))
	}

	// "g" should be A's override.
	if g, ok := nameMap["g"]; !ok {
		t.Fatal("effective missing 'g'")
	} else {
		var cfg map[string]any
		if err := json.Unmarshal(g.Config, &cfg); err != nil {
			t.Fatalf("Unmarshal g.Config: %v", err)
		}
		if cfg["content"] != "A-g" {
			t.Fatalf("g.content = %q, want A-g", cfg["content"])
		}
	}

	// "shared" should be from default.
	if shared, ok := nameMap["shared"]; !ok {
		t.Fatal("effective missing 'shared'")
	} else {
		var cfg map[string]any
		if err := json.Unmarshal(shared.Config, &cfg); err != nil {
			t.Fatalf("Unmarshal shared.Config: %v", err)
		}
		if cfg["content"] != "default-shared" {
			t.Fatalf("shared.content = %q, want default-shared", cfg["content"])
		}
	}

	// "a-only" should be from A.
	if aOnly, ok := nameMap["a-only"]; !ok {
		t.Fatal("effective missing 'a-only'")
	} else {
		var cfg map[string]any
		if err := json.Unmarshal(aOnly.Config, &cfg); err != nil {
			t.Fatalf("Unmarshal a-only.Config: %v", err)
		}
		if cfg["content"] != "A-only" {
			t.Fatalf("a-only.content = %q, want A-only", cfg["content"])
		}
	}
}
