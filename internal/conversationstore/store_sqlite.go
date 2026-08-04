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
	// Normalize zero lifecycle timestamps before persisting so the row never
	// carries the year-1 epoch (matching MemoryStore semantics).
	prepareStoredConversationForMemory(c, time.Now().UTC(), DefaultMemoryStoreTTL)

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

	preserveStoredAt := c.StoredAt.IsZero()
	preserveExpiresAt := c.ExpiresAt.IsZero()
	// Zero timestamps are preserved from the existing row below (never
	// persisted as zero); normalize the remaining values for consistency.
	prepareStoredConversationForMemory(c, time.Now().UTC(), DefaultMemoryStoreTTL)
	storedAt := c.StoredAt.Unix()
	expiresAt := c.ExpiresAt.Unix()

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

// AppendItems atomically appends items to an existing conversation snapshot.
// The append runs in a single UPDATE statement using SQLite's json_insert JSON
// function, which avoids the SELECT+UPDATE TOCTOU race entirely. (The modernc
// driver ignores database/sql transaction isolation hints, so a transaction
// cannot be relied on to serialize writers here.)
func (s *SQLiteStore) AppendItems(ctx context.Context, tenantID, id string, items []json.RawMessage) error {
	if len(items) == 0 {
		return nil
	}

	// Chained json_insert calls append each item in one atomic statement.
	expr := "items"
	args := make([]any, 0, len(items)+2)
	for _, item := range items {
		expr = "json_insert(" + expr + ", '$[#]', json(?))"
		args = append(args, string(item))
	}

	// The CASE branch guards against a NULL/empty items cell (defensive;
	// Create always stores a valid JSON array).
	query := `
		UPDATE conversations
		SET items = CASE
			WHEN items IS NULL OR items = '' THEN ?
			ELSE ` + expr + `
		END
		WHERE tenant_id = ? AND id = ?`
	// The THEN branch stores just the newly appended items as a JSON array.
	newItemsJSON, err := json.Marshal(items)
	if err != nil {
		return fmt.Errorf("marshal new items: %w", err)
	}
	args = append([]any{string(newItemsJSON)}, args...)
	args = append(args, tenantID, id)

	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("append items: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check affected rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
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
