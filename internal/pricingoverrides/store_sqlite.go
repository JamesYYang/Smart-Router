package pricingoverrides

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// SQLiteStore stores model pricing overrides in SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates the model_pricing_overrides table and indexes if needed.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}

	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS model_pricing_overrides (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			selector TEXT NOT NULL,
			provider_name TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			pricing TEXT NOT NULL DEFAULT '{}',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, selector)
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides table: %w", err)
	}

	migrateSQLitePricingOverridesTenantID(db)

	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_model_pricing_overrides_tenant_id ON model_pricing_overrides(tenant_id)`); err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides tenant_id index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_model_pricing_overrides_provider_name ON model_pricing_overrides(tenant_id, provider_name)`); err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides provider_name index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_model_pricing_overrides_model ON model_pricing_overrides(tenant_id, model)`); err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides model index: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_model_pricing_overrides_updated_at ON model_pricing_overrides(tenant_id, updated_at DESC)`); err != nil {
		return nil, fmt.Errorf("failed to create model_pricing_overrides updated_at index: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// migrateSQLitePricingOverridesTenantID adds tenant_id column to existing tables.
func migrateSQLitePricingOverridesTenantID(db *sql.DB) {
	_, err := db.Exec(`ALTER TABLE model_pricing_overrides ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`)
	if err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "duplicate column name") && !strings.Contains(msg, "already exists") {
			if !strings.Contains(msg, "no such table") {
				_ = msg
			}
		}
	}
}

func (s *SQLiteStore) List(ctx context.Context, tenantID string) ([]Override, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tenant_id, selector, provider_name, model, pricing, created_at, updated_at
		FROM model_pricing_overrides
		WHERE tenant_id = ?
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
		override, err := scanSQLiteOverride(rows)
		return override, true, err
	}, rows.Err)
}

func (s *SQLiteStore) ListEffective(ctx context.Context, tenantID string) ([]Override, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT tenant_id, selector, provider_name, model, pricing, created_at, updated_at
		FROM model_pricing_overrides
		WHERE tenant_id IN (?, ?)
		ORDER BY selector ASC, CASE WHEN tenant_id = 'default' THEN 0 ELSE 1 END ASC
	`, "default", tenantID)
	if err != nil {
		return nil, fmt.Errorf("list effective model pricing overrides: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]Override)
	for rows.Next() {
		override, scanErr := scanSQLiteOverride(rows)
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

func (s *SQLiteStore) Upsert(ctx context.Context, tenantID string, override Override) error {
	override, pricingJSON, err := prepareOverrideUpsert(override)
	if err != nil {
		return err
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO model_pricing_overrides (
			tenant_id, selector, provider_name, model, pricing, created_at, updated_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?)
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

func (s *SQLiteStore) Delete(ctx context.Context, tenantID, selector string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM model_pricing_overrides WHERE tenant_id = ? AND selector = ?`, tenantID, strings.TrimSpace(selector))
	if err != nil {
		return fmt.Errorf("delete model pricing override: %w", err)
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

func scanSQLiteOverride(scanner interface{ Scan(dest ...any) error }) (Override, error) {
	var override Override
	var tenantID string
	var pricing string
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
	if err := json.Unmarshal([]byte(pricing), &override.Pricing); err != nil {
		return Override{}, fmt.Errorf("decode pricing: %w", err)
	}
	override.CreatedAt = time.Unix(createdAt, 0).UTC()
	override.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return override, nil
}
