package pricingoverrides

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQLStore stores model pricing overrides in PostgreSQL.
type PostgreSQLStore struct {
	pool *pgxpool.Pool
}

// NewPostgreSQLStore creates the model_pricing_overrides table and indexes if needed.
func NewPostgreSQLStore(ctx context.Context, pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if pool == nil {
		return nil, fmt.Errorf("connection pool is required")
	}

	_, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS model_pricing_overrides (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			selector TEXT NOT NULL,
			provider_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			pricing JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (tenant_id, selector)
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides table: %w", err)
	}

	migratePostgreSQLPricingOverridesTenantID(ctx, pool)

	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_model_pricing_overrides_tenant_id ON model_pricing_overrides(tenant_id)`); err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides tenant_id index: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_model_pricing_overrides_provider_name ON model_pricing_overrides(tenant_id, provider_name)`); err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides provider_name index: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_model_pricing_overrides_model ON model_pricing_overrides(tenant_id, model)`); err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides model index: %w", err)
	}
	if _, err := pool.Exec(ctx, `CREATE INDEX IF NOT EXISTS idx_model_pricing_overrides_updated_at ON model_pricing_overrides(tenant_id, updated_at DESC)`); err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides updated_at index: %w", err)
	}
	return &PostgreSQLStore{pool: pool}, nil
}

// migratePostgreSQLPricingOverridesTenantID adds tenant_id column to existing tables.
func migratePostgreSQLPricingOverridesTenantID(ctx context.Context, pool *pgxpool.Pool) {
	_, err := pool.Exec(ctx, `ALTER TABLE model_pricing_overrides ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`)
	if err != nil {
		// Best-effort migration; table may not exist yet.
		_ = err
	}
}

func (s *PostgreSQLStore) List(ctx context.Context, tenantID string) ([]Override, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, selector, provider_name, model, pricing, created_at, updated_at
		FROM model_pricing_overrides
		WHERE tenant_id = $1
		ORDER BY selector ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list model pricing overrides: %w", err)
	}
	defer rows.Close()
	return collectOverrides(func() (Override, bool, error) {
		if !rows.Next() {
			return Override{}, false, nil
		}
		override, err := scanPostgreSQLOverride(rows)
		return override, true, err
	}, rows.Err)
}

func (s *PostgreSQLStore) ListEffective(ctx context.Context, tenantID string) ([]Override, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT tenant_id, selector, provider_name, model, pricing, created_at, updated_at
		FROM model_pricing_overrides
		WHERE tenant_id IN ('default', $1)
		ORDER BY selector ASC, CASE WHEN tenant_id = 'default' THEN 0 ELSE 1 END ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list effective model pricing overrides: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]Override)
	for rows.Next() {
		override, scanErr := scanPostgreSQLOverride(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		seen[override.Selector] = override
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective model pricing overrides: %w", err)
	}

	result := make([]Override, 0, len(seen))
	for _, override := range seen {
		result = append(result, override)
	}
	return result, nil
}

func (s *PostgreSQLStore) Upsert(ctx context.Context, tenantID string, override Override) error {
	override, pricingJSON, err := prepareOverrideUpsert(override)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO model_pricing_overrides (
			tenant_id, selector, provider_name, model, pricing, created_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7)
		ON CONFLICT(tenant_id, selector) DO UPDATE SET
			provider_name = excluded.provider_name,
			model = excluded.model,
			pricing = excluded.pricing,
			updated_at = excluded.updated_at
	`,
		tenantID,
		override.Selector,
		override.ProviderName,
		override.Model,
		pricingJSON,
		override.CreatedAt.Unix(),
		override.UpdatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("upsert model pricing override: %w", err)
	}
	return nil
}

func (s *PostgreSQLStore) Delete(ctx context.Context, tenantID, selector string) error {
	cmd, err := s.pool.Exec(ctx, `DELETE FROM model_pricing_overrides WHERE tenant_id = $1 AND selector = $2`, tenantID, strings.TrimSpace(selector))
	if err != nil {
		return fmt.Errorf("delete model pricing override: %w", err)
	}
	if cmd.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgreSQLStore) Close() error {
	return nil
}

func scanPostgreSQLOverride(scanner interface{ Scan(dest ...any) error }) (Override, error) {
	var override Override
	var tenantID string
	var pricing []byte
	var createdAt int64
	var updatedAt int64
	if err := scanner.Scan(
		&tenantID,
		&override.Selector,
		&override.ProviderName,
		&override.Model,
		&pricing,
		&createdAt,
		&updatedAt,
	); err != nil {
		return Override{}, fmt.Errorf("scan model pricing override: %w", err)
	}
	if err := json.Unmarshal(pricing, &override.Pricing); err != nil {
		return Override{}, fmt.Errorf("decode pricing: %w", err)
	}
	override.CreatedAt = time.Unix(createdAt, 0).UTC()
	override.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return override, nil
}
