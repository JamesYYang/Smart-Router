package responsestore

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"smartrouter/internal/core"
)

// PostgreSQLStore persists response snapshots in a PostgreSQL database.
type PostgreSQLStore struct {
	pool *pgxpool.Pool
}

// NewPostgreSQLStore creates a new response store backed by a PostgreSQL
// database. It runs CREATE TABLE IF NOT EXISTS and idempotent ALTER TABLE
// migrations.
func NewPostgreSQLStore(ctx context.Context, pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("connection pool is required")
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS stored_responses (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			id TEXT NOT NULL,
			response JSONB NOT NULL,
			input_items JSONB NOT NULL DEFAULT '[]',
			provider TEXT NOT NULL DEFAULT '',
			provider_name TEXT NOT NULL DEFAULT '',
			provider_response_id TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			user_path TEXT NOT NULL DEFAULT '',
			workflow_version_id TEXT NOT NULL DEFAULT '',
			stored_at BIGINT NOT NULL,
			expires_at BIGINT NOT NULL,
			PRIMARY KEY (tenant_id, id)
		)
	`); err != nil {
		return nil, fmt.Errorf("failed to create stored_responses table: %w", err)
	}
	for _, migration := range []string{
		`ALTER TABLE stored_responses ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`,
	} {
		if _, err := pool.Exec(ctx, migration); err != nil {
			return nil, fmt.Errorf("failed to migrate stored_responses table: %w", err)
		}
	}
	return &PostgreSQLStore{pool: pool}, nil
}

// Create inserts a new response snapshot into the PostgreSQL store.
func (s *PostgreSQLStore) Create(ctx context.Context, tenantID string, response *StoredResponse) error {
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

	inputItemsJSON := []byte(`[]`)
	if len(c.InputItems) > 0 {
		inputItemsJSON, err = json.Marshal(c.InputItems)
		if err != nil {
			return fmt.Errorf("marshal input_items: %w", err)
		}
	}

	storedAt := c.StoredAt.Unix()
	expiresAt := c.ExpiresAt.Unix()

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO stored_responses (tenant_id, id, response, input_items, provider, provider_name, provider_response_id, request_id, user_path, workflow_version_id, stored_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, tenantID, c.Response.ID, responseJSON, inputItemsJSON, c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, storedAt, expiresAt)
	if err != nil {
		if isPostgreSQLConstraintError(err) {
			return fmt.Errorf("response already exists: %s", c.Response.ID)
		}
		return fmt.Errorf("create response: %w", err)
	}
	_ = tag
	return nil
}

// Get retrieves a response snapshot by tenant and id.
func (s *PostgreSQLStore) Get(ctx context.Context, tenantID, id string) (*StoredResponse, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT response, input_items, provider, provider_name, provider_response_id, request_id, user_path, workflow_version_id, stored_at, expires_at
		FROM stored_responses
		WHERE tenant_id = $1 AND id = $2
	`, tenantID, id)

	var responseJSON []byte
	var inputItemsJSON []byte
	var provider string
	var providerName string
	var providerResponseID string
	var requestID string
	var userPath string
	var workflowVersionID string
	var storedAt int64
	var expiresAt int64

	if err := row.Scan(&responseJSON, &inputItemsJSON, &provider, &providerName, &providerResponseID, &requestID, &userPath, &workflowVersionID, &storedAt, &expiresAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get response: %w", err)
	}

	var resp core.ResponsesResponse
	if err := json.Unmarshal(responseJSON, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	var inputItems []json.RawMessage
	if err := json.Unmarshal(inputItemsJSON, &inputItems); err != nil {
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
func (s *PostgreSQLStore) Update(ctx context.Context, tenantID string, response *StoredResponse) error {
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

	affected := int64(0)
	if preserveStoredAt && preserveExpiresAt {
		tag, execErr := s.pool.Exec(ctx, `
			UPDATE stored_responses
			SET response = $1, input_items = $2, provider = $3, provider_name = $4, provider_response_id = $5, request_id = $6, user_path = $7, workflow_version_id = $8
			WHERE tenant_id = $9 AND id = $10
		`, responseJSON, inputItemsJSON, c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, tenantID, c.Response.ID)
		affected = tag.RowsAffected()
		err = execErr
	} else if preserveStoredAt {
		tag, execErr := s.pool.Exec(ctx, `
			UPDATE stored_responses
			SET response = $1, input_items = $2, provider = $3, provider_name = $4, provider_response_id = $5, request_id = $6, user_path = $7, workflow_version_id = $8, expires_at = $9
			WHERE tenant_id = $10 AND id = $11
		`, responseJSON, inputItemsJSON, c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, expiresAt, tenantID, c.Response.ID)
		affected = tag.RowsAffected()
		err = execErr
	} else if preserveExpiresAt {
		tag, execErr := s.pool.Exec(ctx, `
			UPDATE stored_responses
			SET response = $1, input_items = $2, provider = $3, provider_name = $4, provider_response_id = $5, request_id = $6, user_path = $7, workflow_version_id = $8, stored_at = $9
			WHERE tenant_id = $10 AND id = $11
		`, responseJSON, inputItemsJSON, c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, storedAt, tenantID, c.Response.ID)
		affected = tag.RowsAffected()
		err = execErr
	} else {
		tag, execErr := s.pool.Exec(ctx, `
			UPDATE stored_responses
			SET response = $1, input_items = $2, provider = $3, provider_name = $4, provider_response_id = $5, request_id = $6, user_path = $7, workflow_version_id = $8, stored_at = $9, expires_at = $10
			WHERE tenant_id = $11 AND id = $12
		`, responseJSON, inputItemsJSON, c.Provider, c.ProviderName, c.ProviderResponseID, c.RequestID, c.UserPath, c.WorkflowVersionID, storedAt, expiresAt, tenantID, c.Response.ID)
		affected = tag.RowsAffected()
		err = execErr
	}
	if err != nil {
		return fmt.Errorf("update response: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes a response snapshot by tenant and id.
func (s *PostgreSQLStore) Delete(ctx context.Context, tenantID, id string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM stored_responses WHERE tenant_id = $1 AND id = $2
	`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete response: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Close is a no-op; DB lifecycle is managed by the shared storage layer.
func (s *PostgreSQLStore) Close() error {
	return nil
}

func isPostgreSQLConstraintError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key") || strings.Contains(message, "unique constraint")
}
