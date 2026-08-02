package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"smartrouter/internal/auditlog"
	"smartrouter/internal/authkeys"
	"smartrouter/internal/core"
)

// BearerTokenAuthenticator authenticates managed bearer tokens and returns
// their internal auth key metadata on success.
type BearerTokenAuthenticator interface {
	Enabled() bool
	Authenticate(ctx context.Context, token string) (authkeys.AuthenticationResult, error)
}

// AuthMiddleware creates an Echo middleware that validates the master key
// if it's configured. If masterKey is empty, no authentication is required.
// skipPaths is a list of paths that should bypass authentication.
func AuthMiddleware(masterKey string, skipPaths []string) echo.MiddlewareFunc {
	return AuthMiddlewareWithAuthenticator(masterKey, nil, skipPaths)
}

// AuthMiddlewareWithAuthenticator validates the legacy master key and, when
// configured, managed auth keys from the auth key service.
func AuthMiddlewareWithAuthenticator(masterKey string, authenticator BearerTokenAuthenticator, skipPaths []string, userPathHeader ...string) echo.MiddlewareFunc {
	userPathHeaderName := configuredUserPathHeaderName(userPathHeader...)
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			// If no auth mechanism is configured, allow all requests (dev mode),
			// except when hitting an admin path on the platform host — without a
			// master key the admin API is unavailable and must surface 503 so the
			// dashboard can guide recovery rather than silently exposing admin.
			if masterKey == "" && (authenticator == nil || !authenticator.Enabled()) {
				if isAdminPath(c.Request().URL.Path) && core.GetPlatformHost(c.Request().Context()) {
					return writeGatewayError(c, (&core.GatewayError{
						Type:       core.ErrorType("master_key_not_configured"),
						Message:    "master key not configured; admin API unavailable on platform host",
						StatusCode: http.StatusServiceUnavailable,
					}).WithCode("master_key_not_configured"))
				}
				auditlog.EnrichEntryWithAuthMethod(c, auditlog.AuthMethodNoKey)
				return next(c)
			}

			// Check if path should skip authentication.
			// Paths ending with "/*" are treated as prefix matches.
			requestPath := c.Request().URL.Path
			for _, skipPath := range skipPaths {
				if strings.HasSuffix(skipPath, "/*") {
					prefix := strings.TrimSuffix(skipPath, "*")
					if strings.HasPrefix(requestPath, prefix) {
						auditlog.EnrichEntryWithAuthMethod(c, auditlog.AuthMethodNoKey)
						return next(c)
					}
				} else if requestPath == skipPath {
					auditlog.EnrichEntryWithAuthMethod(c, auditlog.AuthMethodNoKey)
					return next(c)
				}
			}

			// Get Authorization header
			authHeader := c.Request().Header.Get("Authorization")
			if authHeader == "" {
				authErr := authenticationError(c, "missing authorization header")
				return writeGatewayError(c, authErr)
			}

			// Extract Bearer token
			const prefix = "Bearer "
			if !strings.HasPrefix(authHeader, prefix) {
				authErr := authenticationError(c, "invalid authorization header format, expected 'Bearer <token>'")
				return writeGatewayError(c, authErr)
			}

			token := strings.TrimPrefix(authHeader, prefix)
			if masterKey != "" && subtle.ConstantTimeCompare([]byte(token), []byte(masterKey)) == 1 {
				auditlog.EnrichEntryWithAuthMethod(c, auditlog.AuthMethodMasterKey)
				return next(c)
			}

			if authenticator != nil && authenticator.Enabled() {
				auditlog.EnrichEntryWithAuthMethod(c, auditlog.AuthMethodAPIKey)
				authResult, err := authenticator.Authenticate(c.Request().Context(), token)
				if err == nil {
					applyAuthKeyResult(c, authResult, userPathHeaderName)
					if enforcerErr := enforceTenantAndRole(c, authResult); enforcerErr != nil {
						return writeGatewayError(c, enforcerErr)
					}
					return next(c)
				}

				authErr := authenticationErrorWithAudit(c, authFailureMessage(err), "authentication failed")
				return writeGatewayError(c, authErr)
			}

			authErr := authenticationError(c, "invalid master key")
			return writeGatewayError(c, authErr)
		}
	}
}

// applyAuthKeyResult enriches the request context and audit entry with the
// authenticated managed key's identity, labels, and bound user path.
func applyAuthKeyResult(c *echo.Context, authResult authkeys.AuthenticationResult, userPathHeaderName string) {
	ctx := core.WithAuthKeyID(c.Request().Context(), authResult.ID)
	if len(authResult.Labels) > 0 {
		// Key labels join any labels the tagging middleware already
		// extracted from request headers; duplicates collapse.
		ctx = core.WithRequestLabels(ctx, core.MergeLabels(core.RequestLabelsFromContext(ctx), authResult.Labels))
	}
	if userPath := strings.TrimSpace(authResult.UserPath); userPath != "" {
		ctx = core.WithEffectiveUserPath(ctx, userPath)
		ctx = core.WithUserPathHeaderName(ctx, userPathHeaderName)
		if snapshot := core.GetRequestSnapshot(ctx); snapshot != nil {
			ctx = core.WithRequestSnapshot(ctx, snapshot.WithUserPathHeader(userPath, userPathHeaderName))
		}
		c.Request().Header.Set(userPathHeaderName, userPath)
		auditlog.EnrichEntryWithUserPath(c, userPath)
	}
	c.SetRequest(c.Request().WithContext(ctx))
	auditlog.EnrichEntryWithAuthKeyID(c, authResult.ID)
}

func authFailureMessage(err error) string {
	if err == nil {
		return "invalid API key"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "authentication unavailable"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		return "invalid API key"
	}
	return message
}

func authenticationError(c *echo.Context, message string) *core.GatewayError {
	auditlog.EnrichEntryWithError(c, string(core.ErrorTypeAuthentication), message)
	return core.NewAuthenticationError("", message)
}

func authenticationErrorWithAudit(c *echo.Context, auditMessage, responseMessage string) *core.GatewayError {
	auditlog.EnrichEntryWithError(c, string(core.ErrorTypeAuthentication), auditMessage)
	return core.NewAuthenticationError("", responseMessage)
}

// isAdminPath reports whether the request targets the admin API surface.
// The "/admin/" prefix matches admin endpoints; dashboard/static asset paths
// are separately added to authSkipPaths when the admin UI is enabled.
func isAdminPath(path string) bool {
	return strings.HasPrefix(path, "/admin/")
}

// enforceTenantAndRole applies the two-tier key model after successful
// managed-key authentication. Returns nil to allow, or a *core.GatewayError
// to reject. Rules:
//   - admin path + non-admin key            → 403 insufficient_role
//   - admin path + tenant-admin key on
//     platform host                          → 401 key_not_allowed_on_platform_host
//   - admin path + tenant-admin key on
//     tenant host with ctx tenant mismatch  → 401 key_tenant_mismatch
//   - inference path + ctx tenant mismatch  → 401 key_tenant_mismatch
//
// Master keys are allowed everywhere and never reach this function.
func enforceTenantAndRole(c *echo.Context, result authkeys.AuthenticationResult) *core.GatewayError {
	ctx := c.Request().Context()
	ctxTenantID := core.GetTenantID(ctx)
	isPlatform := core.GetPlatformHost(ctx)
	path := c.Request().URL.Path

	if isAdminPath(path) {
		if !result.IsTenantAdmin {
			return (&core.GatewayError{
				Type:       core.ErrorType("insufficient_role"),
				Message:    "API key does not have admin access",
				StatusCode: http.StatusForbidden,
			}).WithCode("insufficient_role")
		}
		if isPlatform {
			return (&core.GatewayError{
				Type:       core.ErrorType("key_not_allowed_on_platform_host"),
				Message:    "tenant admin key not allowed on platform host",
				StatusCode: http.StatusUnauthorized,
			}).WithCode("key_not_allowed_on_platform_host")
		}
		if ctxTenantID != "" && result.TenantID != "" && result.TenantID != ctxTenantID {
			return (&core.GatewayError{
				Type:       core.ErrorType("key_tenant_mismatch"),
				Message:    "auth key does not belong to this tenant",
				StatusCode: http.StatusUnauthorized,
			}).WithCode("key_tenant_mismatch")
		}
		return nil
	}
	// Inference path.
	if ctxTenantID != "" && result.TenantID != "" && result.TenantID != ctxTenantID {
		return (&core.GatewayError{
			Type:       core.ErrorType("key_tenant_mismatch"),
			Message:    "auth key does not belong to this tenant",
			StatusCode: http.StatusUnauthorized,
		}).WithCode("key_tenant_mismatch")
	}
	return nil
}
