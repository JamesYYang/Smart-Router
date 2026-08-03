package tenants

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgreSQLStore stores tenants in PostgreSQL.
type PostgreSQLStore struct {
	pool *pgxpool.Pool
}

func NewPostgreSQLStore(pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("pgx pool is required")
	}
	_, err := pool.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS tenants (
			id          TEXT PRIMARY KEY,
			subdomain   TEXT NOT NULL UNIQUE,
			name        TEXT NOT NULL,
			status      TEXT NOT NULL DEFAULT 'active',
			plan        TEXT NOT NULL DEFAULT '',
			created_at  BIGINT NOT NULL,
			updated_at  BIGINT NOT NULL
		)
	`)
	if err != nil {
		return nil, fmt.Errorf("create tenants table: %w", err)
	}
	if _, err := pool.Exec(context.Background(), `CREATE INDEX IF NOT EXISTS idx_tenants_status ON tenants(status)`); err != nil {
		return nil, fmt.Errorf("create tenants status index: %w", err)
	}
	return &PostgreSQLStore{pool: pool}, nil
}

func (s *PostgreSQLStore) Create(ctx context.Context, t Tenant) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tenants (id, subdomain, name, status, plan, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, t.ID, t.Subdomain, t.Name, string(t.Status), t.Plan, t.CreatedAt.Unix(), t.UpdatedAt.Unix())
	if err != nil {
		if isPostgreSQLUniqueViolation(err) {
			return fmt.Errorf("tenant subdomain %q already exists: %w", t.Subdomain, ErrSubdomainTaken)
		}
		return fmt.Errorf("create tenant: %w", err)
	}
	return nil
}

func (s *PostgreSQLStore) GetByID(ctx context.Context, id string) (Tenant, error) {
	return scanPGTenant(s.pool.QueryRow(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants WHERE id = $1
	`, id))
}

func (s *PostgreSQLStore) GetBySubdomain(ctx context.Context, sub string) (Tenant, error) {
	return scanPGTenant(s.pool.QueryRow(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants WHERE subdomain = $1
	`, sub))
}

func (s *PostgreSQLStore) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, subdomain, name, status, plan, created_at, updated_at
		FROM tenants ORDER BY created_at DESC, id ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		t, err := scanPGTenant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgreSQLStore) UpdateStatus(ctx context.Context, id string, status Status, updatedAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tenants SET status = $1, updated_at = $2 WHERE id = $3
	`, string(status), updatedAt.Unix(), id)
	if err != nil {
		return fmt.Errorf("update tenant status: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgreSQLStore) Update(ctx context.Context, id, name, plan string, updatedAt time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tenants SET name = $1, plan = $2, updated_at = $3 WHERE id = $4
	`, name, plan, updatedAt.Unix(), id)
	if err != nil {
		return fmt.Errorf("update tenant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgreSQLStore) Close() error { return nil }

func isPostgreSQLUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

type pgScanner interface{ Scan(dest ...any) error }

func scanPGTenant(sc pgScanner) (Tenant, error) {
	var t Tenant
	var status string
	var created, updated int64
	if err := sc.Scan(&t.ID, &t.Subdomain, &t.Name, &status, &t.Plan, &created, &updated); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Tenant{}, ErrNotFound
		}
		return Tenant{}, err
	}
	t.Status = Status(status)
	t.CreatedAt = time.Unix(created, 0).UTC()
	t.UpdatedAt = time.Unix(updated, 0).UTC()
	return t, nil
}
