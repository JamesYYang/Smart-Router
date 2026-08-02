package tagging

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQLStore persists tagging rules in a key-value settings table.
type PostgreSQLStore struct {
	pool *pgxpool.Pool
}

// NewPostgreSQLStore creates the tagging settings table when missing.
func NewPostgreSQLStore(ctx context.Context, pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("connection pool is required")
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS tagging_settings (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (tenant_id, key)
		)
	`); err != nil {
		return nil, fmt.Errorf("failed to create tagging_settings table: %w", err)
	}

	migratePostgreSQLTaggingSettingsTenantID(ctx, pool)

	return &PostgreSQLStore{pool: pool}, nil
}

// migratePostgreSQLTaggingSettingsTenantID adds tenant_id column to existing tables.
func migratePostgreSQLTaggingSettingsTenantID(ctx context.Context, pool *pgxpool.Pool) {
	_, err := pool.Exec(ctx, `ALTER TABLE tagging_settings ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`)
	if err != nil {
		_ = err
	}
}

func (s *PostgreSQLStore) GetRules(ctx context.Context, tenantID string) ([]Rule, error) {
	var value string
	err := s.pool.QueryRow(ctx, `SELECT value FROM tagging_settings WHERE tenant_id = $1 AND key = $2`, tenantID, rulesSettingKey).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get tagging rules: %w", err)
	}
	return decodeRules([]byte(value))
}

func (s *PostgreSQLStore) SaveRules(ctx context.Context, tenantID string, rules []Rule) error {
	value, err := encodeRules(rules)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO tagging_settings (tenant_id, key, value, updated_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at
	`, tenantID, rulesSettingKey, string(value), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("save tagging rules: %w", err)
	}
	return nil
}

func (s *PostgreSQLStore) ListEffectiveRules(ctx context.Context, tenantID string) ([]Rule, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT value
		FROM tagging_settings
		WHERE tenant_id IN ('default', $1) AND key = $2
		ORDER BY CASE WHEN tenant_id = 'default' THEN 0 ELSE 1 END ASC
	`, tenantID, rulesSettingKey)
	if err != nil {
		return nil, fmt.Errorf("list effective tagging rules: %w", err)
	}
	defer rows.Close()

	byHeader := make(map[string]Rule)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("scan effective tagging row: %w", err)
		}
		rules, err := decodeRules([]byte(value))
		if err != nil {
			return nil, fmt.Errorf("decode effective tagging rules: %w", err)
		}
		for _, rule := range rules {
			byHeader[rule.Header] = rule
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective tagging rows: %w", err)
	}

	merged := make([]Rule, 0, len(byHeader))
	for _, rule := range byHeader {
		merged = append(merged, rule)
	}
	return merged, nil
}

// Close is a no-op: the pool is managed by the storage layer.
func (s *PostgreSQLStore) Close() error {
	return nil
}
