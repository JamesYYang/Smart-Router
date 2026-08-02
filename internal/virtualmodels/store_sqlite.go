package virtualmodels

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLiteStore stores virtual models in SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates the virtual_models table and indexes if needed.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}

	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS virtual_models (
			source TEXT NOT NULL,
			tenant_id TEXT NOT NULL DEFAULT 'default',
			targets TEXT NOT NULL DEFAULT '[]',
			strategy TEXT NOT NULL DEFAULT '',
			provider_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			user_paths TEXT NOT NULL DEFAULT '[]',
			description TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, source)
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create virtual_models table: %w", err)
	}

	// Migrate: add tenant_id column to tables created before P3 multi-tenant work.
	migrateSQLiteTenantID(db)

	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_provider_name ON virtual_models(provider_name)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_model ON virtual_models(model)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_enabled ON virtual_models(enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_updated_at ON virtual_models(updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_tenant_id ON virtual_models(tenant_id)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("failed to create virtual_models index: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

// migrateSQLiteTenantID attempts to add the tenant_id column to existing tables
// and ignores the error if the column already exists.
func migrateSQLiteTenantID(db *sql.DB) {
	_, err := db.Exec(`ALTER TABLE virtual_models ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`)
	if err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "duplicate column name") && !strings.Contains(msg, "already exists") {
			// Table may not exist yet (fresh install with CREATE TABLE IF NOT EXISTS),
			// which is fine — the CREATE above handles it.
			if !strings.Contains(msg, "no such table") {
				slogError("virtualmodels: sqlite tenant_id migration failed: " + msg)
			}
		}
	}
}

func slogError(msg string) {
	// Avoid importing slog here; the engine layer logs migrations.
	_ = msg
}

func (s *SQLiteStore) List(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, tenant_id, targets, strategy, provider_name, model, user_paths, description, enabled, created_at, updated_at
		FROM virtual_models
		WHERE tenant_id = ?
		ORDER BY source ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list virtual models: %w", err)
	}
	defer rows.Close()
	return collectVirtualModels(func() (VirtualModel, bool, error) {
		if !rows.Next() {
			return VirtualModel{}, false, nil
		}
		vm, err := scanSQLiteVirtualModel(rows)
		return vm, true, err
	}, rows.Err)
}

func (s *SQLiteStore) ListEffective(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT source, tenant_id, targets, strategy, provider_name, model, user_paths, description, enabled, created_at, updated_at
		FROM virtual_models
		WHERE tenant_id IN (?, ?)
		ORDER BY source ASC, tenant_id ASC
	`, "default", tenantID)
	if err != nil {
		return nil, fmt.Errorf("list effective virtual models: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]VirtualModel)
	for rows.Next() {
		vm, scanErr := scanSQLiteVirtualModel(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		seen[vm.Source] = vm
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective virtual models: %w", err)
	}

	result := make([]VirtualModel, 0, len(seen))
	for _, vm := range seen {
		result = append(result, vm)
	}
	return result, nil
}

func (s *SQLiteStore) Get(ctx context.Context, tenantID, source string) (*VirtualModel, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT source, tenant_id, targets, strategy, provider_name, model, user_paths, description, enabled, created_at, updated_at
		FROM virtual_models
		WHERE tenant_id = ? AND source = ?
	`, tenantID, strings.TrimSpace(source))
	vm, err := scanSQLiteVirtualModel(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &vm, nil
}

const sqliteUpsertVirtualModelSQL = `
		INSERT INTO virtual_models (
			tenant_id, source, targets, strategy, provider_name, model, user_paths, description, enabled, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, source) DO UPDATE SET
			targets = excluded.targets,
			strategy = excluded.strategy,
			provider_name = excluded.provider_name,
			model = excluded.model,
			user_paths = excluded.user_paths,
			description = excluded.description,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at
	`

func sqliteUpsertArgs(tenantID string, vm VirtualModel) ([]any, error) {
	stampUpsert(&vm)
	targetsJSON, err := encodeTargets(vm.Targets)
	if err != nil {
		return nil, err
	}
	pathsJSON, err := encodeUserPaths(vm.UserPaths)
	if err != nil {
		return nil, err
	}
	return []any{
		tenantID,
		strings.TrimSpace(vm.Source),
		targetsJSON,
		vm.Strategy,
		vm.ProviderName,
		vm.Model,
		pathsJSON,
		vm.Description,
		boolToSQLite(vm.Enabled),
		vm.CreatedAt.Unix(),
		vm.UpdatedAt.Unix(),
	}, nil
}

func (s *SQLiteStore) Upsert(ctx context.Context, tenantID string, vm VirtualModel) error {
	args, err := sqliteUpsertArgs(tenantID, vm)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, sqliteUpsertVirtualModelSQL, args...); err != nil {
		return fmt.Errorf("upsert virtual model: %w", err)
	}
	return nil
}

// UpsertAll writes every row in a single transaction, so a failed seed leaves the
// table untouched rather than partially populated (which would otherwise trip the
// "already populated" guard and suppress a re-import on the next start).
func (s *SQLiteStore) UpsertAll(ctx context.Context, tenantID string, vms []VirtualModel) error {
	if len(vms) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin virtual model seed transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, vm := range vms {
		args, err := sqliteUpsertArgs(tenantID, vm)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, sqliteUpsertVirtualModelSQL, args...); err != nil {
			return fmt.Errorf("upsert virtual model: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit virtual model seed: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Delete(ctx context.Context, tenantID, source string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM virtual_models WHERE tenant_id = ? AND source = ?`, tenantID, strings.TrimSpace(source))
	if err != nil {
		return fmt.Errorf("delete virtual model: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read delete rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	return nil
}

func scanSQLiteVirtualModel(scanner interface{ Scan(dest ...any) error }) (VirtualModel, error) {
	var vm VirtualModel
	var targets string
	var userPaths string
	var enabled int
	var createdAt int64
	var updatedAt int64
	var tenantID string
	if err := scanner.Scan(
		&vm.Source,
		&tenantID,
		&targets,
		&vm.Strategy,
		&vm.ProviderName,
		&vm.Model,
		&userPaths,
		&vm.Description,
		&enabled,
		&createdAt,
		&updatedAt,
	); err != nil {
		return VirtualModel{}, err
	}
	var err error
	if vm.Targets, err = decodeTargets([]byte(targets)); err != nil {
		return VirtualModel{}, err
	}
	if vm.UserPaths, err = decodeUserPaths([]byte(userPaths)); err != nil {
		return VirtualModel{}, err
	}
	vm.Enabled = enabled != 0
	vm.CreatedAt = time.Unix(createdAt, 0).UTC()
	vm.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return vm, nil
}

func boolToSQLite(v bool) int {
	if v {
		return 1
	}
	return 0
}
