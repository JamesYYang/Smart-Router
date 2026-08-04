package responsestore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"smartrouter/internal/core"
)

// SQLiteStore persists response snapshots in a SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates a new response store backed by a SQLite database.
// It runs CREATE TABLE IF NOT EXISTS and idempotent ALTER TABLE migrations.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS stored_responses (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			id TEXT NOT NULL,
			response TEXT NOT NULL,
			input_items TEXT NOT NULL DEFAULT '[]',
			provider TEXT NOT NULL DEFAULT '',
			provider_name TEXT NOT NULL DEFAULT '',
			provider_response_id TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			user_path TEXT NOT NULL DEFAULT '',
			workflow_version_id TEXT NOT NULL DEFAULT '',
			stored_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, id)
		)
	`); err != nil {
		return nil, fmt.Errorf("failed to create stored_responses table: %w", err)
	}
	for _, migration := range []string{
		`ALTER TABLE stored_responses ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	} {
		if _, err := db.Exec(migration); err != nil && !isSQLiteDuplicateColumnError(err) {
			return nil, fmt.Errorf("failed to migrate stored_responses table: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

// Create inserts a new response snapshot into the SQLite store.
func (s *SQLiteStore) Create(ctx context.Context, tenantID string, response *StoredResponse) error {
	if response == nil || response.Response == nil || response.Response.ID == "" {
		return fmt.Errorf("response id is required")
	}

	c, err := cloneResponse(response)
	if err != nil {
		return err
	}
	c.TenantID = tenantID
	// Normalize zero lifecycle timestamps before persisting so the row never
	// carries the year-1 epoch (matching MemoryStore semantics).
	prepareStoredResponseForMemory(c, time.Now().UTC(), DefaultMemoryStoreTTL)

	responseJSON, err := json.Marshal(c.Response)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}

	inputItemsJSON := []byte(json.RawMessage(`[]`))
	if len(c.InputItems) > 0 {
		inputItemsJSON, err = json.Marshal(c.InputItems)
		if err != nil {
			return fmt.Errorf("marshal input_items: %w", err)
		}
	}

	storedAt := c.StoredAt.Unix()
	expiresAt := c.ExpiresAt.Unix()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO stored_responses (tenant_id, id, response, input_items, provider, provider_name, provider_response_id, request_id, user_path, workflow_version_id, stored_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, tenantID, c.Response.ID, string(responseJSON), string(inputItemsJSON), c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, storedAt, expiresAt)
	if err != nil {
		if isSQLiteConstraintError(err) {
			return fmt.Errorf("response already exists: %s", c.Response.ID)
		}
		return fmt.Errorf("create response: %w", err)
	}
	_ = result
	return nil
}

// Get retrieves a response snapshot by tenant and id.
func (s *SQLiteStore) Get(ctx context.Context, tenantID, id string) (*StoredResponse, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT response, input_items, provider, provider_name, provider_response_id, request_id, user_path, workflow_version_id, stored_at, expires_at
		FROM stored_responses
		WHERE tenant_id = ? AND id = ?
	`, tenantID, id)

	var responseJSON string
	var inputItemsJSON string
	var provider string
	var providerName string
	var providerResponseID string
	var requestID string
	var userPath string
	var workflowVersionID string
	var storedAt int64
	var expiresAt int64

	if err := row.Scan(&responseJSON, &inputItemsJSON, &provider, &providerName, &providerResponseID, &requestID, &userPath, &workflowVersionID, &storedAt, &expiresAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get response: %w", err)
	}

	var resp core.ResponsesResponse
	if err := json.Unmarshal([]byte(responseJSON), &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	var inputItems []json.RawMessage
	if err := json.Unmarshal([]byte(inputItemsJSON), &inputItems); err != nil {
		return nil, fmt.Errorf("unmarshal input_items: %w", err)
	}

	return &StoredResponse{
		Response:           &resp,
		InputItems:         inputItems,
		Provider:           provider,
		ProviderName:       providerName,
		ProviderResponseID: providerResponseID,
		RequestID:          requestID,
		UserPath:           userPath,
		WorkflowVersionID:  workflowVersionID,
		TenantID:           tenantID,
		StoredAt:           time.Unix(storedAt, 0).UTC(),
		ExpiresAt:          time.Unix(expiresAt, 0).UTC(),
	}, nil
}

// Update replaces an existing response snapshot.
// Preserves stored_at and expires_at from the existing row when the incoming
// values are zero (matching MemoryStore semantics).
func (s *SQLiteStore) Update(ctx context.Context, tenantID string, response *StoredResponse) error {
	if response == nil || response.Response == nil || response.Response.ID == "" {
		return fmt.Errorf("response id is required")
	}

	c, err := cloneResponse(response)
	if err != nil {
		return err
	}

	responseJSON, err := json.Marshal(c.Response)
	if err != nil {
		return fmt.Errorf("marshal response: %w", err)
	}

	inputItemsJSON := []byte(`[]`)
	if len(c.InputItems) > 0 {
		inputItemsJSON, err = json.Marshal(c.InputItems)
		if err != nil {
			return fmt.Errorf("marshal input_items: %w", err)
		}
	}

	storedAt := c.StoredAt.Unix()
	expiresAt := c.ExpiresAt.Unix()
	preserveStoredAt := c.StoredAt.IsZero()
	preserveExpiresAt := c.ExpiresAt.IsZero()
	// Zero timestamps are preserved from the existing row below (never
	// persisted as zero); normalize the remaining values for consistency.
	prepareStoredResponseForMemory(c, time.Now().UTC(), DefaultMemoryStoreTTL)
	storedAt = c.StoredAt.Unix()
	expiresAt = c.ExpiresAt.Unix()

	var result sql.Result
	if preserveStoredAt && preserveExpiresAt {
		result, err = s.db.ExecContext(ctx, `
			UPDATE stored_responses
			SET response = ?, input_items = ?, provider = ?, provider_name = ?, provider_response_id = ?, request_id = ?, user_path = ?, workflow_version_id = ?
			WHERE tenant_id = ? AND id = ?
		`, string(responseJSON), string(inputItemsJSON), c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, tenantID, c.Response.ID)
	} else if preserveStoredAt {
		result, err = s.db.ExecContext(ctx, `
			UPDATE stored_responses
			SET response = ?, input_items = ?, provider = ?, provider_name = ?, provider_response_id = ?, request_id = ?, user_path = ?, workflow_version_id = ?, expires_at = ?
			WHERE tenant_id = ? AND id = ?
		`, string(responseJSON), string(inputItemsJSON), c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, expiresAt, tenantID, c.Response.ID)
	} else if preserveExpiresAt {
		result, err = s.db.ExecContext(ctx, `
			UPDATE stored_responses
			SET response = ?, input_items = ?, provider = ?, provider_name = ?, provider_response_id = ?, request_id = ?, user_path = ?, workflow_version_id = ?, stored_at = ?
			WHERE tenant_id = ? AND id = ?
		`, string(responseJSON), string(inputItemsJSON), c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, storedAt, tenantID, c.Response.ID)
	} else {
		result, err = s.db.ExecContext(ctx, `
			UPDATE stored_responses
			SET response = ?, input_items = ?, provider = ?, provider_name = ?, provider_response_id = ?, request_id = ?, user_path = ?, workflow_version_id = ?, stored_at = ?, expires_at = ?
			WHERE tenant_id = ? AND id = ?
		`, string(responseJSON), string(inputItemsJSON), c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, storedAt, expiresAt, tenantID, c.Response.ID)
	}
	if err != nil {
		return fmt.Errorf("update response: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a response snapshot by tenant and id.
func (s *SQLiteStore) Delete(ctx context.Context, tenantID, id string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM stored_responses WHERE tenant_id = ? AND id = ?
	`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete response: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Close is a no-op; DB lifecycle is managed by the shared storage layer.
func (s *SQLiteStore) Close() error {
	return nil
}

func isSQLiteDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate column") || strings.Contains(message, "already exists")
}

func isSQLiteConstraintError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "unique constraint") || strings.Contains(message, "constraint failed")
}
