package guardrails

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLiteStore stores guardrail definitions in SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates the guardrail table and indexes if needed.
func NewSQLiteStore(ctx context.Context, db *sql.DB) (*SQLiteStore, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS guardrail_definitions (
			name TEXT NOT NULL,
			tenant_id TEXT NOT NULL DEFAULT 'default',
			type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			user_path TEXT,
			config JSON NOT NULL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, name)
		)`,
		`ALTER TABLE guardrail_definitions ADD COLUMN user_path TEXT`,
		`ALTER TABLE guardrail_definitions ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
		`CREATE INDEX IF NOT EXISTS idx_guardrail_definitions_type ON guardrail_definitions(type)`,
		`CREATE INDEX IF NOT EXISTS idx_guardrail_definitions_updated_at ON guardrail_definitions(updated_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			if strings.HasPrefix(statement, `ALTER TABLE guardrail_definitions ADD COLUMN`) && isSQLiteDuplicateColumnError(err) {
				continue
			}
			return nil, fmt.Errorf("initialize guardrail definitions table: %w", err)
		}
	}

	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) List(ctx context.Context, tenantID string) ([]Definition, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, type, description, user_path, config, created_at, updated_at
		FROM guardrail_definitions
		WHERE tenant_id = ?
		ORDER BY name ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list guardrails: %w", err)
	}
	defer rows.Close()
	return collectDefinitions(rows, scanSQLiteDefinition)
}

func (s *SQLiteStore) ListEffective(ctx context.Context, tenantID string) ([]Definition, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, type, description, user_path, config, created_at, updated_at
		FROM guardrail_definitions
		WHERE tenant_id IN (?, ?)
		ORDER BY name ASC, CASE WHEN tenant_id = 'default' THEN 0 ELSE 1 END ASC
	`, "default", tenantID)
	if err != nil {
		return nil, fmt.Errorf("list effective guardrails: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]Definition)
	for rows.Next() {
		definition, scanErr := scanSQLiteDefinition(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		seen[definition.Name] = definition
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate effective guardrails: %w", err)
	}

	result := make([]Definition, 0, len(seen))
	for _, definition := range seen {
		result = append(result, definition)
	}
	return result, nil
}

func (s *SQLiteStore) Get(ctx context.Context, tenantID, name string) (*Definition, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT name, type, description, user_path, config, created_at, updated_at
		FROM guardrail_definitions
		WHERE tenant_id = ? AND name = ?
	`, tenantID, normalizeDefinitionName(name))
	definition, err := scanSQLiteDefinition(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &definition, nil
}

func (s *SQLiteStore) Upsert(ctx context.Context, tenantID string, definition Definition) error {
	definition, err := normalizeDefinition(definition)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Unix()
	if definition.CreatedAt.IsZero() {
		definition.CreatedAt = time.Unix(now, 0).UTC()
	}
	definition.UpdatedAt = time.Unix(now, 0).UTC()

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO guardrail_definitions (name, tenant_id, type, description, user_path, config, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
			type = excluded.type,
			description = excluded.description,
			user_path = excluded.user_path,
			config = excluded.config,
			updated_at = excluded.updated_at
	`, definition.Name, tenantID, definition.Type, definition.Description, nullableString(definition.UserPath), string(definition.Config), definition.CreatedAt.Unix(), definition.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert guardrail: %w", err)
	}
	return nil
}

func (s *SQLiteStore) UpsertMany(ctx context.Context, tenantID string, definitions []Definition) error {
	if len(definitions) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin guardrail upsert transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	now := time.Now().UTC().Unix()
	for _, definition := range definitions {
		normalized, err := normalizeDefinition(definition)
		if err != nil {
			return err
		}
		if normalized.CreatedAt.IsZero() {
			normalized.CreatedAt = time.Unix(now, 0).UTC()
		}
		normalized.UpdatedAt = time.Unix(now, 0).UTC()

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO guardrail_definitions (name, tenant_id, type, description, user_path, config, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(tenant_id, name) DO UPDATE SET
				type = excluded.type,
				description = excluded.description,
				user_path = excluded.user_path,
				config = excluded.config,
				updated_at = excluded.updated_at
		`, normalized.Name, tenantID, normalized.Type, normalized.Description, nullableString(normalized.UserPath), string(normalized.Config), normalized.CreatedAt.Unix(), normalized.UpdatedAt.Unix()); err != nil {
			return fmt.Errorf("upsert guardrail %q: %w", normalized.Name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit guardrail upsert transaction: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Delete(ctx context.Context, tenantID, name string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM guardrail_definitions WHERE tenant_id = ? AND name = ?`, tenantID, normalizeDefinitionName(name))
	if err != nil {
		return fmt.Errorf("delete guardrail: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete guardrail rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	return nil
}

func scanSQLiteDefinition(scanner definitionScanner) (Definition, error) {
	var (
		definition    Definition
		userPath      sql.NullString
		configJSON    string
		createdAtUnix int64
		updatedAtUnix int64
	)
	if err := scanner.Scan(
		&definition.Name,
		&definition.Type,
		&definition.Description,
		&userPath,
		&configJSON,
		&createdAtUnix,
		&updatedAtUnix,
	); err != nil {
		return Definition{}, err
	}
	definition.UserPath = nullableStringValue(userPath)
	definition.Config = append([]byte(nil), configJSON...)
	definition.CreatedAt = time.Unix(createdAtUnix, 0).UTC()
	definition.UpdatedAt = time.Unix(updatedAtUnix, 0).UTC()
	return definition, nil
}

func isSQLiteDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	if strings.Contains(message, "duplicate column") || strings.Contains(message, "duplicate column name") {
		return true
	}
	return strings.Contains(message, "already exists") && strings.Contains(message, "column")
}
