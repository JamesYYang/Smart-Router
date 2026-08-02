package guardrails

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQLStore stores guardrail definitions in PostgreSQL.
type PostgreSQLStore struct {
	pool *pgxpool.Pool
}

// NewPostgreSQLStore creates the guardrail table and indexes if needed.
func NewPostgreSQLStore(ctx context.Context, pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if ctx == nil {
		return nil, fmt.Errorf("context is required")
	}
	if pool == nil {
		return nil, fmt.Errorf("connection pool is required")
	}

	statements := []string{
		`CREATE TABLE IF NOT EXISTS guardrail_definitions (
			name TEXT NOT NULL,
			tenant_id TEXT NOT NULL DEFAULT 'default',
			type TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			user_path TEXT,
			config JSONB NOT NULL,
			created_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			PRIMARY KEY (tenant_id, name)
		)`,
		`ALTER TABLE guardrail_definitions ADD COLUMN IF NOT EXISTS user_path TEXT`,
		`ALTER TABLE guardrail_definitions ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`,
		`CREATE INDEX IF NOT EXISTS idx_guardrail_definitions_type ON guardrail_definitions(type)`,
		`CREATE INDEX IF NOT EXISTS idx_guardrail_definitions_updated_at ON guardrail_definitions(updated_at DESC)`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			return nil, fmt.Errorf("initialize guardrail definitions table: %w", err)
		}
	}

	return &PostgreSQLStore{pool: pool}, nil
}

func (s *PostgreSQLStore) List(ctx context.Context, tenantID string) ([]Definition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, type, description, user_path, config, created_at, updated_at
		FROM guardrail_definitions
		WHERE tenant_id = $1
		ORDER BY name ASC
	`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list guardrails: %w", err)
	}
	defer rows.Close()
	return collectDefinitions(rows, scanPostgreSQLDefinition)
}

func (s *PostgreSQLStore) ListEffective(ctx context.Context, tenantID string) ([]Definition, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, type, description, user_path, config, created_at, updated_at
		FROM guardrail_definitions
		WHERE tenant_id IN ($1, $2)
		ORDER BY name ASC, CASE WHEN tenant_id = 'default' THEN 0 ELSE 1 END ASC
	`, "default", tenantID)
	if err != nil {
		return nil, fmt.Errorf("list effective guardrails: %w", err)
	}
	defer rows.Close()

	seen := make(map[string]Definition)
	for rows.Next() {
		definition, scanErr := scanPostgreSQLDefinition(rows)
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

func (s *PostgreSQLStore) Get(ctx context.Context, tenantID, name string) (*Definition, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT name, type, description, user_path, config, created_at, updated_at
		FROM guardrail_definitions
		WHERE tenant_id = $1 AND name = $2
	`, tenantID, normalizeDefinitionName(name))
	definition, err := scanPostgreSQLDefinition(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &definition, nil
}

func (s *PostgreSQLStore) Upsert(ctx context.Context, tenantID string, definition Definition) error {
	definition, err := normalizeDefinition(definition)
	if err != nil {
		return err
	}

	now := time.Now().UTC().Unix()
	if definition.CreatedAt.IsZero() {
		definition.CreatedAt = time.Unix(now, 0).UTC()
	}
	definition.UpdatedAt = time.Unix(now, 0).UTC()

	_, err = s.pool.Exec(ctx, `
		INSERT INTO guardrail_definitions (name, tenant_id, type, description, user_path, config, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT(tenant_id, name) DO UPDATE SET
			type = excluded.type,
			description = excluded.description,
			user_path = excluded.user_path,
			config = excluded.config,
			updated_at = excluded.updated_at
	`, definition.Name, tenantID, definition.Type, definition.Description, nullableString(definition.UserPath), definition.Config, definition.CreatedAt.Unix(), definition.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert guardrail: %w", err)
	}
	return nil
}

func (s *PostgreSQLStore) UpsertMany(ctx context.Context, tenantID string, definitions []Definition) error {
	if len(definitions) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin guardrail upsert transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
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

		if _, err := tx.Exec(ctx, `
			INSERT INTO guardrail_definitions (name, tenant_id, type, description, user_path, config, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT(tenant_id, name) DO UPDATE SET
				type = excluded.type,
				description = excluded.description,
				user_path = excluded.user_path,
				config = excluded.config,
				updated_at = excluded.updated_at
		`, normalized.Name, tenantID, normalized.Type, normalized.Description, nullableString(normalized.UserPath), normalized.Config, normalized.CreatedAt.Unix(), normalized.UpdatedAt.Unix()); err != nil {
			return fmt.Errorf("upsert guardrail %q: %w", normalized.Name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit guardrail upsert transaction: %w", err)
	}
	return nil
}

func (s *PostgreSQLStore) Delete(ctx context.Context, tenantID, name string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM guardrail_definitions WHERE tenant_id = $1 AND name = $2`, tenantID, normalizeDefinitionName(name))
	if err != nil {
		return fmt.Errorf("delete guardrail: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgreSQLStore) Close() error {
	return nil
}

func scanPostgreSQLDefinition(scanner definitionScanner) (Definition, error) {
	var (
		definition    Definition
		userPath      sql.NullString
		configJSON    []byte
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
