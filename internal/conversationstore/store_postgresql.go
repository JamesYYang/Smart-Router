package conversationstore

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

// PostgreSQLStore persists conversations in a PostgreSQL database.
type PostgreSQLStore struct {
	pool *pgxpool.Pool
}

// NewPostgreSQLStore creates a new conversation store backed by a PostgreSQL
// database. It runs CREATE TABLE IF NOT EXISTS and idempotent ALTER TABLE
// migrations.
func NewPostgreSQLStore(ctx context.Context, pool *pgxpool.Pool) (*PostgreSQLStore, error) {
	if pool == nil {
		return nil, fmt.Errorf("connection pool is required")
	}
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS conversations (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			id TEXT NOT NULL,
			conversation JSONB NOT NULL,
			items JSONB NOT NULL DEFAULT '[]',
			user_path TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			stored_at BIGINT NOT NULL,
			expires_at BIGINT NOT NULL,
			PRIMARY KEY (tenant_id, id)
		)
	`); err != nil {
		return nil, fmt.Errorf("failed to create conversations table: %w", err)
	}
	for _, migration := range []string{
		`ALTER TABLE conversations ADD COLUMN IF NOT EXISTS tenant_id TEXT NOT NULL DEFAULT 'default'`,
	} {
		if _, err := pool.Exec(ctx, migration); err != nil {
			return nil, fmt.Errorf("failed to migrate conversations table: %w", err)
		}
	}
	return &PostgreSQLStore{pool: pool}, nil
}

// Create inserts a new conversation snapshot into the PostgreSQL store.
func (s *PostgreSQLStore) Create(ctx context.Context, tenantID string, conversation *StoredConversation) error {
	if conversation == nil || conversation.Conversation == nil || conversation.Conversation.ID == "" {
		return fmt.Errorf("conversation id is required")
	}

	c, err := cloneConversation(conversation)
	if err != nil {
		return err
	}
	c.TenantID = tenantID
	// Normalize zero lifecycle timestamps before persisting so the row never
	// carries the year-1 epoch (matching MemoryStore semantics).
	prepareStoredConversationForMemory(c, time.Now().UTC(), DefaultMemoryStoreTTL)

	convJSON, err := json.Marshal(c.Conversation)
	if err != nil {
		return fmt.Errorf("marshal conversation: %w", err)
	}

	itemsJSON := []byte(`[]`)
	if len(c.Items) > 0 {
		itemsJSON, err = json.Marshal(c.Items)
		if err != nil {
			return fmt.Errorf("marshal items: %w", err)
		}
	}

	storedAt := c.StoredAt.Unix()
	expiresAt := c.ExpiresAt.Unix()

	tag, err := s.pool.Exec(ctx, `
		INSERT INTO conversations (tenant_id, id, conversation, items, user_path, request_id, stored_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, tenantID, c.Conversation.ID, convJSON, itemsJSON, c.UserPath, c.RequestID, storedAt, expiresAt)
	if err != nil {
		if isPostgreSQLConstraintError(err) {
			return fmt.Errorf("conversation already exists: %s", c.Conversation.ID)
		}
		return fmt.Errorf("create conversation: %w", err)
	}
	_ = tag
	return nil
}

// Get retrieves a conversation snapshot by tenant and id.
func (s *PostgreSQLStore) Get(ctx context.Context, tenantID, id string) (*StoredConversation, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT conversation, items, user_path, request_id, stored_at, expires_at
		FROM conversations
		WHERE tenant_id = $1 AND id = $2
	`, tenantID, id)

	var conversationJSON []byte
	var itemsJSON []byte
	var userPath string
	var requestID string
	var storedAt int64
	var expiresAt int64

	if err := row.Scan(&conversationJSON, &itemsJSON, &userPath, &requestID, &storedAt, &expiresAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get conversation: %w", err)
	}

	var conv core.Conversation
	if err := json.Unmarshal(conversationJSON, &conv); err != nil {
		return nil, fmt.Errorf("unmarshal conversation: %w", err)
	}

	var items []json.RawMessage
	if err := json.Unmarshal(itemsJSON, &items); err != nil {
		return nil, fmt.Errorf("unmarshal items: %w", err)
	}

	return &StoredConversation{
		Conversation: &conv,
		Items:        items,
		UserPath:     userPath,
		RequestID:    requestID,
		TenantID:     tenantID,
		StoredAt:     time.Unix(storedAt, 0).UTC(),
		ExpiresAt:    time.Unix(expiresAt, 0).UTC(),
	}, nil
}

// Update replaces an existing conversation snapshot.
// Preserves stored_at and expires_at from the existing row when the incoming
// values are zero (matching MemoryStore semantics).
func (s *PostgreSQLStore) Update(ctx context.Context, tenantID string, conversation *StoredConversation) error {
	if conversation == nil || conversation.Conversation == nil || conversation.Conversation.ID == "" {
		return fmt.Errorf("conversation id is required")
	}

	c, err := cloneConversation(conversation)
	if err != nil {
		return err
	}

	convJSON, err := json.Marshal(c.Conversation)
	if err != nil {
		return fmt.Errorf("marshal conversation: %w", err)
	}

	itemsJSON := []byte(`[]`)
	if len(c.Items) > 0 {
		itemsJSON, err = json.Marshal(c.Items)
		if err != nil {
			return fmt.Errorf("marshal items: %w", err)
		}
	}

	storedAt := c.StoredAt.Unix()
	expiresAt := c.ExpiresAt.Unix()
	preserveStoredAt := c.StoredAt.IsZero()
	preserveExpiresAt := c.ExpiresAt.IsZero()
	// Zero timestamps are preserved from the existing row below (never
	// persisted as zero); normalize the remaining values for consistency.
	prepareStoredConversationForMemory(c, time.Now().UTC(), DefaultMemoryStoreTTL)
	storedAt = c.StoredAt.Unix()
	expiresAt = c.ExpiresAt.Unix()

	affected := int64(0)
	if preserveStoredAt && preserveExpiresAt {
		tag, execErr := s.pool.Exec(ctx, `
			UPDATE conversations
			SET conversation = $1, items = $2, user_path = $3, request_id = $4
			WHERE tenant_id = $5 AND id = $6
		`, convJSON, itemsJSON, c.UserPath, c.RequestID, tenantID, c.Conversation.ID)
		affected = tag.RowsAffected()
		err = execErr
	} else if preserveStoredAt {
		tag, execErr := s.pool.Exec(ctx, `
			UPDATE conversations
			SET conversation = $1, items = $2, user_path = $3, request_id = $4, expires_at = $5
			WHERE tenant_id = $6 AND id = $7
		`, convJSON, itemsJSON, c.UserPath, c.RequestID, expiresAt, tenantID, c.Conversation.ID)
		affected = tag.RowsAffected()
		err = execErr
	} else if preserveExpiresAt {
		tag, execErr := s.pool.Exec(ctx, `
			UPDATE conversations
			SET conversation = $1, items = $2, user_path = $3, request_id = $4, stored_at = $5
			WHERE tenant_id = $6 AND id = $7
		`, convJSON, itemsJSON, c.UserPath, c.RequestID, storedAt, tenantID, c.Conversation.ID)
		affected = tag.RowsAffected()
		err = execErr
	} else {
		tag, execErr := s.pool.Exec(ctx, `
			UPDATE conversations
			SET conversation = $1, items = $2, user_path = $3, request_id = $4, stored_at = $5, expires_at = $6
			WHERE tenant_id = $7 AND id = $8
		`, convJSON, itemsJSON, c.UserPath, c.RequestID, storedAt, expiresAt, tenantID, c.Conversation.ID)
		affected = tag.RowsAffected()
		err = execErr
	}
	if err != nil {
		return fmt.Errorf("update conversation: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendItems atomically appends items to an existing conversation snapshot
// using a SELECT+UPDATE transaction.
func (s *PostgreSQLStore) AppendItems(ctx context.Context, tenantID, id string, items []json.RawMessage) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin append items: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var existingItemsJSON []byte
	if err := tx.QueryRow(ctx, `
		SELECT items FROM conversations WHERE tenant_id = $1 AND id = $2
	`, tenantID, id).Scan(&existingItemsJSON); err != nil {
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("select items for append: %w", err)
	}

	var existingItems []json.RawMessage
	if err := json.Unmarshal(existingItemsJSON, &existingItems); err != nil {
		return fmt.Errorf("unmarshal existing items: %w", err)
	}

	for _, item := range items {
		existingItems = append(existingItems, core.CloneRawJSON(item))
	}

	updatedJSON, err := json.Marshal(existingItems)
	if err != nil {
		return fmt.Errorf("marshal updated items: %w", err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE conversations SET items = $1 WHERE tenant_id = $2 AND id = $3
	`, updatedJSON, tenantID, id)
	if err != nil {
		return fmt.Errorf("update items: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit append items: %w", err)
	}
	return nil
}

// Delete removes a conversation snapshot by tenant and id.
func (s *PostgreSQLStore) Delete(ctx context.Context, tenantID, id string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM conversations WHERE tenant_id = $1 AND id = $2
	`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
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
