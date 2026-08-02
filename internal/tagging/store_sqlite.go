package tagging

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"
)

// SQLiteStore persists tagging rules in a key-value settings table.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates the tagging settings table when missing.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tagging_settings (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, key)
		)
	`); err != nil {
		return nil, fmt.Errorf("failed to create tagging_settings table: %w", err)
	}

	migrateSQLiteTaggingSettingsTenantID(db)

	return &SQLiteStore{db: db}, nil
}

// migrateSQLiteTaggingSettingsTenantID adds tenant_id column to existing tables.
func migrateSQLiteTaggingSettingsTenantID(db *sql.DB) {
	_, err := db.Exec(`ALTER TABLE tagging_settings ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`)
	if err != nil {
		msg := err.Error()
		if !strings.Contains(msg, "duplicate column name") && !strings.Contains(msg, "already exists") {
			if !strings.Contains(msg, "no such table") {
				_ = msg
			}
		}
	}
}

func (s *SQLiteStore) GetRules(ctx context.Context, tenantID string) ([]Rule, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM tagging_settings WHERE tenant_id = ? AND key = ?`, tenantID, rulesSettingKey).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get tagging rules: %w", err)
	}
	return decodeRules([]byte(value))
}

func (s *SQLiteStore) SaveRules(ctx context.Context, tenantID string, rules []Rule) error {
	value, err := encodeRules(rules)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO tagging_settings (tenant_id, key, value, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(tenant_id, key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
	`, tenantID, rulesSettingKey, string(value), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("save tagging rules: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListEffectiveRules(ctx context.Context, tenantID string) ([]Rule, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT value
		FROM tagging_settings
		WHERE tenant_id IN (?, ?) AND key = ?
		ORDER BY CASE WHEN tenant_id = 'default' THEN 0 ELSE 1 END ASC
	`, "default", tenantID, rulesSettingKey)
	if err != nil {
		return nil, fmt.Errorf("list effective tagging rules: %w", err)
	}
	defer rows.Close()

	// Merge: defaults first, then tenant rules overwrite by header.
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
		return nil, fmt.Errorf("iterate effective tagging rules: %w", err)
	}

	merged := make([]Rule, 0, len(byHeader))
	for _, rule := range byHeader {
		merged = append(merged, rule)
	}
	return merged, nil
}

// Close is a no-op: the DB handle is managed by the storage layer.
func (s *SQLiteStore) Close() error {
	return nil
}

func encodeRules(rules []Rule) ([]byte, error) {
	if rules == nil {
		rules = []Rule{}
	}
	value, err := json.Marshal(rules)
	if err != nil {
		return nil, fmt.Errorf("encode tagging rules: %w", err)
	}
	return value, nil
}

func decodeRules(value []byte) ([]Rule, error) {
	if len(value) == 0 {
		return nil, nil
	}
	var rules []Rule
	if err := json.Unmarshal(value, &rules); err != nil {
		return nil, fmt.Errorf("decode tagging rules: %w", err)
	}
	return rules, nil
}
