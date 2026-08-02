package virtualmodels

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQLStore stores virtual models in PostgreSQL.
type PostgreSQLStore struct {
	pool *pgxpool.Pool
}

// NewPostgreSQLStore creates the virtual_models table and indexes if needed.
func NewPostgreSQLStore(ctx context.Context, pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if pool == nil {
		return nil, fmt.Errorf("connection pool is required")
	}

	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS virtual_models (
			source TEXT NOT NULL,
			tenant_id TEXT NOT NULL DEFAULT 'default',
			targets JSONB NOT NULL DEFAULT '[]'::jsonb,
			strategy TEXT NOT NULL DEFAULT '',
			provider_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			user_paths JSONB NOT NULL DEFAULT '[]'::jsonb,
			description TEXT NOT NULL DEFAULT '',
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (tenant_id, source)
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create virtual_models table: %w", err)
	}

	// Migrate: add tenant_id column to tables created before P3 multi-tenant work.
	_, _ = pool.Exec(ctx, `ALTER TABLE virtual_models ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`)

	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_provider_name ON virtual_models(provider_name)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_model ON virtual_models(model)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_enabled ON virtual_models(enabled)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_updated_at ON virtual_models(updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_virtual_models_tenant_id ON virtual_models(tenant_id)`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return nil, fmt.Errorf("failed to create virtual_models index: %w", err)
		}
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func (s *PostgreSQLStore) List(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, tenant_id, targets, strategy, provider_name, model, user_paths, description, enabled, created_at, updated_at
		FROM virtual_models
		WHERE tenant_id = $1
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
		vm, err := scanPostgreSQLVirtualModel(rows)
		return vm, true, err
	}, rows.Err)
}

func (s *PostgreSQLStore) ListEffective(ctx context.Context, tenantID string) ([]VirtualModel, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT source, tenant_id, targets, strategy, provider_name, model, user_paths, description, enabled, created_at, updated_at
		FROM virtual_models
		WHERE tenant_id IN ($1, $2)
		ORDER BY source ASC, tenant_id ASC
	`, "default", tenantID)
	if err != nil {
		return nil, fmt.Errorf("list effective virtual models: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]VirtualModel)
	for rows.Next() {
		vm, scanErr := scanPostgreSQLVirtualModel(rows)
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

func (s *PostgreSQLStore) Get(ctx context.Context, tenantID, source string) (*VirtualModel, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT source, tenant_id, targets, strategy, provider_name, model, user_paths, description, enabled, created_at, updated_at
		FROM virtual_models
		WHERE tenant_id = $1 AND source = $2
	`, tenantID, strings.TrimSpace(source))
	vm, err := scanPostgreSQLVirtualModel(row)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &vm, nil
}

const postgresUpsertVirtualModelSQL = `
		INSERT INTO virtual_models (
			tenant_id, source, targets, strategy, provider_name, model, user_paths, description, enabled, created_at, updated_at
		)
		VALUES ($1, $2, $3::jsonb, $4, $5, $6, $7::jsonb, $8, $9, $10, $11)
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

func postgresUpsertArgs(tenantID string, vm VirtualModel) ([]any, error) {
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
		vm.Enabled,
		vm.CreatedAt.Unix(),
		vm.UpdatedAt.Unix(),
	}, nil
}

func (s *PostgreSQLStore) Upsert(ctx context.Context, tenantID string, vm VirtualModel) error {
	args, err := postgresUpsertArgs(tenantID, vm)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, postgresUpsertVirtualModelSQL, args...); err != nil {
		return fmt.Errorf("upsert virtual model: %w", err)
	}
	return nil
}

// UpsertAll writes every row in a single transaction, so a failed seed leaves the
// table untouched rather than partially populated (which would otherwise trip the
// "already populated" guard and suppress a re-import on the next start).
func (s *PostgreSQLStore) UpsertAll(ctx context.Context, tenantID string, vms []VirtualModel) error {
	if len(vms) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin virtual model seed transaction: %w", err)
	}
	defer func() {
		rollbackCtx, cancel := rollbackContext()
		defer cancel()
		_ = tx.Rollback(rollbackCtx)
	}()
	for _, vm := range vms {
		args, err := postgresUpsertArgs(tenantID, vm)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, postgresUpsertVirtualModelSQL, args...); err != nil {
			return fmt.Errorf("upsert virtual model: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit virtual model seed: %w", err)
	}
	return nil
}

func (s *PostgreSQLStore) Delete(ctx context.Context, tenantID, source string) error {
	cmd, err := s.pool.Exec(ctx, `DELETE FROM virtual_models WHERE tenant_id = $1 AND source = $2`, tenantID, strings.TrimSpace(source))
	if err != nil {
		return fmt.Errorf("delete virtual model: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgreSQLStore) Close() error {
	return nil
}

func scanPostgreSQLVirtualModel(scanner interface{ Scan(dest ...any) error }) (VirtualModel, error) {
	var vm VirtualModel
	var targets []byte
	var userPaths []byte
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
		&vm.Enabled,
		&createdAt,
		&updatedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return VirtualModel{}, ErrNotFound
		}
		return VirtualModel{}, fmt.Errorf("scan virtual model: %w", err)
	}
	var err error
	if vm.Targets, err = decodeTargets(targets); err != nil {
		return VirtualModel{}, err
	}
	if vm.UserPaths, err = decodeUserPaths(userPaths); err != nil {
		return VirtualModel{}, err
	}
	vm.CreatedAt = time.Unix(createdAt, 0).UTC()
	vm.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return vm, nil
}
