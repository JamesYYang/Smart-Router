package conversationstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/goccy/go-json"

	"smartrouter/internal/core"
)

// SQLiteStore persists conversations in a SQLite database.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore creates a new conversation store backed by a SQLite database.
// It runs CREATE TABLE IF NOT EXISTS and idempotent ALTER TABLE migrations.
func NewSQLiteStore(db *sql.DB) (*SQLiteStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS conversations (
			tenant_id TEXT NOT NULL DEFAULT 'default',
			id TEXT NOT NULL,
			conversation TEXT NOT NULL,
			items TEXT NOT NULL DEFAULT '[]',
			user_path TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			stored_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			PRIMARY KEY (tenant_id, id)
		)
	`); err != nil {
		return nil, fmt.Errorf("failed to create conversations table: %w", err)
	}
	for _, migration := range []string{
		`ALTER TABLE conversations ADD COLUMN tenant_id TEXT NOT NULL DEFAULT 'default'`,
	} {
		if _, err := db.Exec(migration); err != nil && !isSQLiteDuplicateColumnError(err) {
			return nil, fmt.Errorf("failed to migrate conversations table: %w", err)
		}
	}
	return &SQLiteStore{db: db}, nil
}

// Create inserts a new conversation snapshot into the SQLite store.
func (s *SQLiteStore) Create(ctx context.Context, tenantID string, conversation *StoredConversation) error {
	if conversation == nil || conversation.Conversation == nil || conversation.Conversation.ID == "" {
		return fmt.Errorf("conversation id is required")
	}

	c, err := cloneConversation(conversation)
	if err != nil {
		return err
	}
	c.TenantID = tenantID

	convJSON, err := json.Marshal(c.Conversation)
	if err != nil {
		return fmt.Errorf("marshal conversation: %w", err)
	}

	itemsJSON := []byte(json.RawMessage(`[]`))
	if len(c.Items) > 0 {
		itemsJSON, err = json.Marshal(c.Items)
		if err != nil {
			return fmt.Errorf("marshal items: %w", err)
		}
	}

	storedAt := c.StoredAt.Unix()
	expiresAt := c.ExpiresAt.Unix()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO conversations (tenant_id, id, conversation, items, user_path, request_id, stored_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, tenantID, c.Conversation.ID, string(convJSON), string(itemsJSON), c.UserPath, c.RequestID, storedAt, expiresAt)
	if err != nil {
		if isSQLiteConstraintError(err) {
			return fmt.Errorf("conversation already exists: %s", c.Conversation.ID)
		}
		return fmt.Errorf("create conversation: %w", err)
	}
	_ = result
	return nil
}

// Get retrieves a conversation snapshot by tenant and id.
func (s *SQLiteStore) Get(ctx context.Context, tenantID, id string) (*StoredConversation, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT conversation, items, user_path, request_id, stored_at, expires_at
		FROM conversations
		WHERE tenant_id = ? AND id = ?
	`, tenantID, id)

	var conversationJSON string
	var itemsJSON string
	var userPath string
	var requestID string
	var storedAt int64
	var expiresAt int64

	if err := row.Scan(&conversationJSON, &itemsJSON, &userPath, &requestID, &storedAt, &expiresAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get conversation: %w", err)
	}

	var conv core.Conversation
	if err := json.Unmarshal([]byte(conversationJSON), &conv); err != nil {
		return nil, fmt.Errorf("unmarshal conversation: %w", err)
	}

	var items []json.RawMessage
	if err := json.Unmarshal([]byte(itemsJSON), &items); err != nil {
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
func (s *SQLiteStore) Update(ctx context.Context, tenantID string, conversation *StoredConversation) error {
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

	var result sql.Result
	if preserveStoredAt && preserveExpiresAt {
		result, err = s.db.ExecContext(ctx, `
			UPDATE conversations
			SET conversation = ?, items = ?, user_path = ?, request_id = ?
			WHERE tenant_id = ? AND id = ?
		`, string(convJSON), string(itemsJSON), c.UserPath, c.RequestID, tenantID, c.Conversation.ID)
	} else if preserveStoredAt {
		result, err = s.db.ExecContext(ctx, `
			UPDATE conversations
			SET conversation = ?, items = ?, user_path = ?, request_id = ?, expires_at = ?
			WHERE tenant_id = ? AND id = ?
		`, string(convJSON), string(itemsJSON), c.UserPath, c.RequestID, expiresAt, tenantID, c.Conversation.ID)
	} else if preserveExpiresAt {
		result, err = s.db.ExecContext(ctx, `
			UPDATE conversations
			SET conversation = ?, items = ?, user_path = ?, request_id = ?, stored_at = ?
			WHERE tenant_id = ? AND id = ?
		`, string(convJSON), string(itemsJSON), c.UserPath, c.RequestID, storedAt, tenantID, c.Conversation.ID)
	} else {
		result, err = s.db.ExecContext(ctx, `
			UPDATE conversations
			SET conversation = ?, items = ?, user_path = ?, request_id = ?, stored_at = ?, expires_at = ?
			WHERE tenant_id = ? AND id = ?
		`, string(convJSON), string(itemsJSON), c.UserPath, c.RequestID, storedAt, expiresAt, tenantID, c.Conversation.ID)
	}
	if err != nil {
		return fmt.Errorf("update conversation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// AppendItems atomically appends items to an existing conversation snapshot
// using a SELECT+UPDATE transaction.
func (s *SQLiteStore) AppendItems(ctx context.Context, tenantID, id string, items []json.RawMessage) error {
	if len(items) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin append items: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var existingItemsJSON string
	if err := tx.QueryRowContext(ctx, `
		SELECT items FROM conversations WHERE tenant_id = ? AND id = ?
	`, tenantID, id).Scan(&existingItemsJSON); err != nil {
		if err == sql.ErrNoRows {
			return ErrNotFound
		}
		return fmt.Errorf("select items for append: %w", err)
	}

	var existingItems []json.RawMessage
	if err := json.Unmarshal([]byte(existingItemsJSON), &existingItems); err != nil {
		return fmt.Errorf("unmarshal existing items: %w", err)
	}

	for _, item := range items {
		existingItems = append(existingItems, core.CloneRawJSON(item))
	}

	updatedJSON, err := json.Marshal(existingItems)
	if err != nil {
		return fmt.Errorf("marshal updated items: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE conversations SET items = ? WHERE tenant_id = ? AND id = ?
	`, string(updatedJSON), tenantID, id)
	if err != nil {
		return fmt.Errorf("update items: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check affected rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit append items: %w", err)
	}
	return nil
}

// Delete removes a conversation snapshot by tenant and id.
func (s *SQLiteStore) Delete(ctx context.Context, tenantID, id string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM conversations WHERE tenant_id = ? AND id = ?
	`, tenantID, id)
	if err != nil {
		return fmt.Errorf("delete conversation: %w", err)
	}
	affected, err := result.RowsAffected()
	if err == nil && affected == 0 {
		return ErrNotFound
	}
	return nil
}

// Close closes the underlying database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
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
