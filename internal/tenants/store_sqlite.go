package tenants

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLiteStore stores tenants in SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates the tenants table and indexes if needed.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS tenants (
			id          TEXT PRIMARY KEY,
			subdomain   TEXT NOT NULL UNIQUE,
			name        TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'active',
			plan        TEXT NOT NULL DEFAULT '',
			created_at  INTEGER NOT NULL,
			updated_at  INTEGER NOT NULL
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("failed to create tenants table: %w", err)
	}
	for _, idx := range []string{
		`CREATE INDEX IF NOT EXISTS idx_tenants_subdomain ON tenants(subdomain)`,
		`CREATE INDEX IF NOT EXISTS idx_tenants_status ON tenants(status)`,
	} {
		if _, err := db.Exec(idx); err != nil {
			return nil, fmt.Errorf("failed to create tenants index: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Create(ctx context.Context, tenant Tenant) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tenants (id, subdomain, name, status, plan, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, tenant.ID, tenant.Subdomain, tenant.Name, string(tenant.Status), tenant.Plan, tenant.CreatedAt.Unix(), tenant.UpdatedAt.Unix())
	if err != nil {
		if isSQLiteUniqueConstraintError(err) {
			return fmt.Errorf("tenant subdomain %q already exists: %w", tenant.Subdomain, err)
		}
		return fmt.Errorf("create tenant: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetByID(ctx context.Context, id string) (Tenant, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants WHERE id = ?
	`, id)
	return scanSQLiteTenant(row)
}

func (s *SQLiteStore) GetBySubdomain(ctx context.Context, subdomain string) (Tenant, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants WHERE subdomain = ?
	`, subdomain)
	return scanSQLiteTenant(row)
}

func (s *SQLiteStore) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants ORDER BY created_at DESC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanSQLiteTenant(rows)
		if err != nil {
			return nil, fmt.Errorf("iterate tenants: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) UpdateStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE tenants SET status = ?, updated_at = ? WHERE id = ?
	`, string(status), updatedAt.Unix(), id)
	if err != nil {
		return fmt.Errorf("update tenant status: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read update status rows affected: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) Close() error { return nil }

type sqliteScanner interface{ Scan(dest ...any) error }

func scanSQLiteTenant(scanner sqliteScanner) (Tenant, error) {
	var t Tenant
	var status string
	var createdAt, updatedAt int64
	if err := scanner.Scan(&t.ID, &t.Subdomain, &t.Name, &status, &t.Plan, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Tenant{}, ErrNotFound
		}
		return Tenant{}, err
	}
	t.Status = Status(status)
	t.CreatedAt = time.Unix(createdAt, 0).UTC()
	t.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return t, nil
}

func isSQLiteUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique") || strings.Contains(msg, "constraint")
}
