package authkeys

import "time"

const (
	// TokenPrefix is the managed API key prefix returned to clients.
	TokenPrefix = "sk_gom_"
	secretBytes = 32
)

// AuthKey is the persisted auth key record.
type AuthKey struct {
	ID            string     `json:"id" bson:"_id"`
	Name          string     `json:"name" bson:"name"`
	Description   string     `json:"description,omitempty" bson:"description,omitempty"`
	UserPath      string     `json:"user_path,omitempty" bson:"user_path,omitempty"`
	Labels        []string   `json:"labels,omitempty" bson:"labels,omitempty"`
	RedactedValue string     `json:"redacted_value" bson:"redacted_value"`
	SecretHash    string     `json:"-" bson:"secret_hash"`
	Enabled       bool       `json:"enabled" bson:"enabled"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty" bson:"expires_at,omitempty"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty" bson:"deactivated_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at" bson:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at" bson:"updated_at"`
	// TenantID binds this key to a tenant. Empty means no tenant binding
	// (legacy / dev mode). Post-P2 migration all keys should have a value.
	TenantID string `json:"tenant_id,omitempty" bson:"tenant_id,omitempty"`
	// IsTenantAdmin allows this key to access /admin/* on its tenant host.
	// Platform admin creates these; tenant admins create regular API keys.
	IsTenantAdmin bool `json:"is_tenant_admin,omitempty" bson:"is_tenant_admin,omitempty"`
}

// View is the admin-facing representation of a managed auth key.
type View struct {
	AuthKey
	Active bool `json:"active"`
}

// IssuedKey is returned once on create and includes the plaintext token value.
type IssuedKey struct {
	View
	Value string `json:"value"`
}

// CreateInput captures the admin request for issuing a new auth key.
type CreateInput struct {
	Name          string
	Description   string
	UserPath      string
	Labels        []string
	ExpiresAt     *time.Time
	TenantID      string
	IsTenantAdmin bool
}

// Active reports whether the key can currently authenticate requests.
func (k AuthKey) Active(now time.Time) bool {
	if !k.Enabled {
		return false
	}
	if k.DeactivatedAt != nil {
		return false
	}
	if k.ExpiresAt != nil && !k.ExpiresAt.After(now) {
		return false
	}
	return true
}
