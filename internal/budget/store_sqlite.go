package budget

import (
	"smartrouter/internal/storage/sqlutil"

	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	settingDailyResetHour     = "daily_reset_hour"
	settingDailyResetMinute   = "daily_reset_minute"
	settingWeeklyResetWeekday = "weekly_reset_weekday"
	settingWeeklyResetHour    = "weekly_reset_hour"
	settingWeeklyResetMinute  = "weekly_reset_minute"
	settingMonthlyResetDay    = "monthly_reset_day"
	settingMonthlyResetHour   = "monthly_reset_hour"
	settingMonthlyResetMinute = "monthly_reset_minute"
)

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS budgets (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			user_path TEXT NOT NULL,
			period_seconds INTEGER NOT NULL,
			amount REAL NOT NULL,
			source TEXT NOT NULL DEFAULT '',
			last_reset_at INTEGER,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, user_path, period_seconds)
		)
	`); err != nil {
		return nil, fmt.Errorf("failed to create budgets table: %w", err)
	}
	for _, migration := range []string{
		`ALTER TABLE budgets ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE budgets ADD COLUMN last_reset_at INTEGER`,
		`ALTER TABLE budgets ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	} {
		if _, err := db.Exec(migration); err != nil && !isSQLiteDuplicateColumnError(err) {
			return nil, fmt.Errorf("failed to migrate budgets table: %w", err)
		}
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS budget_settings (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			key TEXT NOT NULL,
			value TEXT NOT NULL,
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, key)
		)
	`); err != nil {
		return nil, fmt.Errorf("failed to create budget_settings table: %w", err)
	}
	for _, migration := range []string{
		`ALTER TABLE budget_settings ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	} {
		if _, err := db.Exec(migration); err != nil && !isSQLiteDuplicateColumnError(err) {
			return nil, fmt.Errorf("failed to migrate budget_settings table: %w", err)
		}
	}
	for _, index := range []string{
		`CREATE INDEX IF NOT EXISTS idx_budgets_user_path ON budgets(user_path)`,
		`CREATE INDEX IF NOT EXISTS idx_budgets_period_seconds ON budgets(period_seconds)`,
	} {
		if _, err := db.Exec(index); err != nil {
			return nil, fmt.Errorf("failed to create budget index: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) ListBudgets(ctx context.Context, tenantID string) ([]Budget, error) {
	query := `SELECT user_path, period_seconds, amount, source, last_reset_at, created_at, updated_at FROM budgets`
	var args []any
	if tenantID != "" {
		query += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	query += ` ORDER BY user_path ASC, period_seconds ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	defer rows.Close()

	var budgets []Budget
	for rows.Next() {
		budget, err := scanSQLiteBudget(rows)
		if err != nil {
			return nil, err
		}
		budgets = append(budgets, budget)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate budgets: %w", err)
	}
	return budgets, nil
}

func (s *SQLiteStore) UpsertBudgets(ctx context.Context, tenantID string, budgets []Budget) error {
	budgets, err := normalizeBudgetsForUpsert(budgets)
	if err != nil {
		return err
	}
	if len(budgets) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin budget upsert: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO budgets (tenant_id, user_path, period_seconds, amount, source, last_reset_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, user_path, period_seconds) DO UPDATE SET
			amount = CASE WHEN excluded.source = ? OR budgets.source = ? THEN excluded.amount ELSE budgets.amount END,
			source = CASE WHEN excluded.source = ? OR budgets.source = ? THEN excluded.source ELSE budgets.source END,
			updated_at = CASE WHEN excluded.source = ? OR budgets.source = ? THEN excluded.updated_at ELSE budgets.updated_at END
	`)
	if err != nil {
		return fmt.Errorf("prepare budget upsert: %w", err)
	}
	defer stmt.Close()

	for _, budget := range budgets {
		if _, err := stmt.ExecContext(
			ctx,
			tenantID,
			budget.UserPath,
			budget.PeriodSeconds,
			budget.Amount,
			budget.Source,
			sqlutil.UnixOrNil(budget.LastResetAt),
			budget.CreatedAt.Unix(),
			budget.UpdatedAt.Unix(),
			SourceManual,
			SourceConfig,
			SourceManual,
			SourceConfig,
			SourceManual,
			SourceConfig,
		); err != nil {
			return fmt.Errorf("upsert budget %s/%d: %w", budget.UserPath, budget.PeriodSeconds, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit budget upsert: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteBudget(ctx context.Context, tenantID, userPath string, periodSeconds int64) error {
	userPath, err := NormalizeUserPath(userPath)
	if err != nil {
		return err
	}
	if periodSeconds <= 0 {
		return fmt.Errorf("period_seconds must be greater than 0")
	}
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM budgets
		WHERE tenant_id = ? AND user_path = ? AND period_seconds = ?
	`, tenantID, userPath, periodSeconds)
	if err != nil {
		return fmt.Errorf("delete budget %s/%d: %w", userPath, periodSeconds, err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return fmt.Errorf("%w: %s/%d", ErrNotFound, userPath, periodSeconds)
	}
	return nil
}

func (s *SQLiteStore) ReplaceConfigBudgets(ctx context.Context, tenantID string, budgets []Budget) error {
	budgets, err := normalizeBudgetsForUpsert(budgets)
	if err != nil {
		return err
	}
	for i := range budgets {
		budgets[i].Source = SourceConfig
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin config budget replace: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if len(budgets) == 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM budgets WHERE tenant_id = ? AND source = ?`, tenantID, SourceConfig); err != nil {
			return fmt.Errorf("delete old config budgets: %w", err)
		}
	} else {
		conditions := make([]string, 0, len(budgets))
		args := make([]any, 0, 2+len(budgets)*2)
		args = append(args, tenantID, SourceConfig)
		for _, budget := range budgets {
			conditions = append(conditions, `(user_path = ? AND period_seconds = ?)`)
			args = append(args, budget.UserPath, budget.PeriodSeconds)
		}
		query := `DELETE FROM budgets WHERE tenant_id = ? AND source = ? AND NOT (` + strings.Join(conditions, " OR ") + `)`
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("delete old config budgets: %w", err)
		}
	}
	if err := upsertSQLiteBudgets(ctx, tx, tenantID, budgets); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit config budget replace: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetSettings(ctx context.Context, tenantID string) (Settings, error) {
	query := `SELECT key, value, updated_at FROM budget_settings`
	var args []any
	if tenantID != "" {
		query += ` WHERE tenant_id = ?`
		args = append(args, tenantID)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return Settings{}, fmt.Errorf("get budget settings: %w", err)
	}
	defer rows.Close()

	return scanSettingsRows(rows)
}

func (s *SQLiteStore) SaveSettings(ctx context.Context, tenantID string, settings Settings) (Settings, error) {
	if err := ValidateSettings(settings); err != nil {
		return Settings{}, err
	}
	settings.UpdatedAt = time.Now().UTC()
	values := settingsKeyValues(settings)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Settings{}, fmt.Errorf("begin budget settings save: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO budget_settings (tenant_id, key, value, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(tenant_id, key) DO UPDATE SET
			value = excluded.value,
			updated_at = excluded.updated_at
	`)
	if err != nil {
		return Settings{}, fmt.Errorf("prepare budget settings save: %w", err)
	}
	defer stmt.Close()

	for key, value := range values {
		if _, err := stmt.ExecContext(ctx, tenantID, key, strconv.Itoa(value), settings.UpdatedAt.Unix()); err != nil {
			return Settings{}, fmt.Errorf("save budget setting %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return Settings{}, fmt.Errorf("commit budget settings save: %w", err)
	}
	return settings, nil
}

func (s *SQLiteStore) ResetBudget(ctx context.Context, tenantID, userPath string, periodSeconds int64, at time.Time) error {
	userPath, err := NormalizeUserPath(userPath)
	if err != nil {
		return err
	}
	if periodSeconds <= 0 {
		return fmt.Errorf("period_seconds must be greater than 0")
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE budgets
		SET last_reset_at = ?, updated_at = ?
		WHERE tenant_id = ? AND user_path = ? AND period_seconds = ?
	`, at.UTC().Unix(), at.UTC().Unix(), tenantID, userPath, periodSeconds)
	if err != nil {
		return fmt.Errorf("reset budget %s/%d: %w", userPath, periodSeconds, err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return fmt.Errorf("%w: %s/%d", ErrNotFound, userPath, periodSeconds)
	}
	return nil
}

func (s *SQLiteStore) ResetAllBudgets(ctx context.Context, tenantID string, at time.Time) error {
	var err error
	if tenantID != "" {
		_, err = s.db.ExecContext(ctx, `UPDATE budgets SET last_reset_at = ?, updated_at = ? WHERE tenant_id = ?`, at.UTC().Unix(), at.UTC().Unix(), tenantID)
	} else {
		_, err = s.db.ExecContext(ctx, `UPDATE budgets SET last_reset_at = ?, updated_at = ?`, at.UTC().Unix(), at.UTC().Unix())
	}
	if err != nil {
		return fmt.Errorf("reset all budgets: %w", err)
	}
	return nil
}

func (s *SQLiteStore) SumUsageCost(ctx context.Context, tenantID, userPath string, start, end time.Time) (float64, bool, error) {
	userPath, err := NormalizeUserPath(userPath)
	if err != nil {
		return 0, false, err
	}
	// "*" / "/*" means tenant-aggregate: sum usage across all user paths.
	isAggregate := userPath == "*" || userPath == "/*"
	query := `SELECT SUM(total_cost) FROM usage
		WHERE ` + sqliteTimestampEpochExpr() + ` >= unixepoch(?)
			AND ` + sqliteTimestampEpochExpr() + ` < unixepoch(?)
			AND (cache_type IS NULL OR cache_type = '')`
	args := []any{
		start.UTC().Format(time.RFC3339Nano),
		end.UTC().Format(time.RFC3339Nano),
	}
	if !isAggregate {
		userPathExpr := usagePathMatchesBudgetExpr("user_path")
		query += ` AND (` + userPathExpr + ` = ? OR ` + userPathExpr + ` LIKE ? ESCAPE '\')`
		args = append(args, userPath, usagePathLikePattern(userPath))
	}
	if tenantID != "" {
		query += ` AND tenant_id = ?`
		args = append(args, tenantID)
	}
	var total sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return 0, false, fmt.Errorf("sum usage cost: %w", err)
	}
	if !total.Valid {
		return 0, false, nil
	}
	return total.Float64, true, nil
}

func (s *SQLiteStore) Close() error {
	return nil
}

func upsertSQLiteBudgets(ctx context.Context, tx *sql.Tx, tenantID string, budgets []Budget) error {
	if len(budgets) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO budgets (tenant_id, user_path, period_seconds, amount, source, last_reset_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(tenant_id, user_path, period_seconds) DO UPDATE SET
			amount = CASE WHEN excluded.source = ? OR budgets.source = ? THEN excluded.amount ELSE budgets.amount END,
			source = CASE WHEN excluded.source = ? OR budgets.source = ? THEN excluded.source ELSE budgets.source END,
			updated_at = CASE WHEN excluded.source = ? OR budgets.source = ? THEN excluded.updated_at ELSE budgets.updated_at END
	`)
	if err != nil {
		return fmt.Errorf("prepare budget upsert: %w", err)
	}
	defer stmt.Close()

	for _, budget := range budgets {
		if _, err := stmt.ExecContext(
			ctx,
			tenantID,
			budget.UserPath,
			budget.PeriodSeconds,
			budget.Amount,
			budget.Source,
			sqlutil.UnixOrNil(budget.LastResetAt),
			budget.CreatedAt.Unix(),
			budget.UpdatedAt.Unix(),
			SourceManual,
			SourceConfig,
			SourceManual,
			SourceConfig,
			SourceManual,
			SourceConfig,
		); err != nil {
			return fmt.Errorf("upsert budget %s/%d: %w", budget.UserPath, budget.PeriodSeconds, err)
		}
	}
	return nil
}

func scanSQLiteBudget(scanner interface{ Scan(dest ...any) error }) (Budget, error) {
	var budget Budget
	var lastResetAt sql.NullInt64
	var createdAt int64
	var updatedAt int64
	if err := scanner.Scan(
		&budget.UserPath,
		&budget.PeriodSeconds,
		&budget.Amount,
		&budget.Source,
		&lastResetAt,
		&createdAt,
		&updatedAt,
	); err != nil {
		return Budget{}, fmt.Errorf("scan budget: %w", err)
	}
	budget.LastResetAt = sqlutil.TimeFromUnix(lastResetAt)
	budget.CreatedAt = time.Unix(createdAt, 0).UTC()
	budget.UpdatedAt = time.Unix(updatedAt, 0).UTC()
	return budget, nil
}

func sqliteTimestampEpochExpr() string {
	return "unixepoch(REPLACE(timestamp, ' ', 'T'))"
}

func isSQLiteDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate column") || strings.Contains(message, "already exists")
}
