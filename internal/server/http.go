package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"path"
	"strings"
	"time"

	"smartrouter/config"
	"smartrouter/ext"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"smartrouter/internal/admin"
	"smartrouter/internal/admin/dashboard"
	"smartrouter/internal/auditlog"
	batchstore "smartrouter/internal/batch"
	"smartrouter/internal/conversationstore"
	"smartrouter/internal/core"
	"smartrouter/internal/filestore"
	"smartrouter/internal/responsecache"
	"smartrouter/internal/responsestore"
	"smartrouter/internal/tagging"
	"smartrouter/internal/tenants"
	"smartrouter/internal/usage"
)

// Server wraps the Echo server
type Server struct {
	echo                    *echo.Echo
	handler                 *Handler
	responseCacheMiddleware *responsecache.ResponseCacheMiddleware
	responseStore           responsestore.Store
	conversationStore       conversationstore.Store
}

const (
	inboundServerReadTimeout       = 30 * time.Second
	inboundServerReadHeaderTimeout = 10 * time.Second
	inboundServerWriteTimeout      = 30 * time.Second
)

// Config holds server configuration options
type Config struct {
	BasePath                        string                                 // URL path prefix where the app is mounted (default: /)
	MasterKey                       string                                 // Optional: Master key for authentication
	Authenticator                   BearerTokenAuthenticator               // Optional: managed API key authenticator
	MetricsEnabled                  bool                                   // Whether to expose Prometheus metrics endpoint
	MetricsEndpoint                 string                                 // HTTP path for metrics endpoint (default: /metrics)
	BodySizeLimit                   string                                 // Max request body size (e.g., "10M", "1024K")
	PprofEnabled                    bool                                   // Whether to expose debug profiling routes at /debug/pprof/*
	AuditLogger                     auditlog.LoggerInterface               // Optional: Audit logger for request/response logging
	UsageLogger                     usage.LoggerInterface                  // Optional: Usage logger for token tracking
	BudgetChecker                   BudgetChecker                          // Optional: per-user-path budget checker
	PricingResolver                 usage.PricingResolver                  // Optional: Resolves pricing for cost calculation
	ModelResolver                   RequestModelResolver                   // Optional: explicit model resolver used during workflow resolution
	ModelAuthorizer                 RequestModelAuthorizer                 // Optional: request-scoped concrete model access controller
	WorkflowPolicyResolver          RequestWorkflowPolicyResolver          // Optional: persisted workflow resolver used during workflow resolution
	FailoverResolver                RequestFailoverResolver                // Optional: translated-route failover resolver
	TranslatedRequestPatcher        TranslatedRequestPatcher               // Optional: request patcher for translated routes after workflow resolution
	BatchRequestPreparer            BatchRequestPreparer                   // Optional: batch request preparer before native provider submission
	ExposedModelLister              ExposedModelLister                     // Optional: additional public models to merge into GET /v1/models
	KeepOnlyAliasesAtModelsEndpoint bool                                   // Whether GET /v1/models should hide concrete provider models
	PassthroughSemanticEnrichers    []core.PassthroughSemanticEnricher     // Optional: provider-owned passthrough semantic enrichers before workflow resolution
	BatchStore                      batchstore.Store                       // Optional: Batch lifecycle persistence store
	FileStore                       filestore.Store                        // Optional: File provider mapping persistence store
	ResponseStore                   responsestore.Store                    // Optional: Responses lifecycle persistence store
	ConversationStore               conversationstore.Store                // Optional: Conversations lifecycle persistence store
	LogOnlyModelInteractions        bool                                   // Only log AI model endpoints (default: true)
	DisablePassthroughRoutes        bool                                   // Disable /p/{provider}/{endpoint} route registration
	RealtimeEnabled                 bool                                   // Enable realtime websocket route /v1/realtime and passthrough upgrades
	EnabledPassthroughProviders     []string                               // Provider types enabled on /p/{provider}/... passthrough routes
	AllowPassthroughV1Alias         *bool                                  // Allow /p/{provider}/v1/... aliases; nil defaults to true
	UserPathHeader                  string                                 // Header carrying the request user path (default: X-GoModel-User-Path)
	AdminEndpointsEnabled           bool                                   // Whether admin API endpoints are enabled
	AdminUIEnabled                  bool                                   // Whether admin dashboard UI is enabled
	PlatformAdminHandler            *admin.PlatformAdminHandler            // Serves /admin/* on the platform host (nil if disabled)
	TenantAdminHandler              *admin.TenantAdminHandler              // Serves /admin/* on each tenant's own host (nil if disabled)
	DashboardHandler                *dashboard.Handler                     // Dashboard UI handler (nil if disabled)
	SwaggerEnabled                  bool                                   // Whether to expose the Swagger UI at /swagger/index.html
	ResponseCacheMiddleware         *responsecache.ResponseCacheMiddleware // Optional: response cache middleware for cacheable endpoints
	GuardrailsHash                  string                                 // Optional: SHA-256 hash of active guardrail rules; stored in context post-patch for semantic cache
	IPExtractor                     echo.IPExtractor                       // Optional: trusted client IP extraction strategy for proxied deployments
	StorageProbe                    ReadinessProbe                         // Optional: primary storage connectivity check; failure makes /health/ready report not_ready (503)
	CacheProbe                      ReadinessProbe                         // Optional: Redis cache connectivity check; failure makes /health/ready report degraded (200, non-blocking)
	RequestRewriters                []ext.RequestRewriter                  // Optional: raw-body rewriters invoked on inference ingress (post-auth, pre-workflow-resolution)
	ExtraMiddleware                 []echo.MiddlewareFunc                  // Optional: extension middleware registered after audit, before gateway auth
	ExtraRoutes                     []func(*echo.Echo)                     // Optional: extension route registration callbacks invoked after core routes
	ExtraAuthSkipPaths              []string                               // Optional: extension paths appended to the auth skip list ("/*" suffix matches a prefix)
	Tagging                         *tagging.Service                       // Optional: request labelling based on configured tagging headers
	// Tenants is the tenant resolution service. When non-nil and BaseDomain
	// is set, the TenantResolver middleware runs before RequestSnapshotCapture.
	Tenants      *tenants.Service
	BaseDomain   string
	PlatformHost string
}

// ReadinessProbe verifies that a dependency the gateway owns is reachable.
// It is deliberately narrow so the server stays decoupled from concrete storage
// and cache types. Upstream provider reachability is intentionally NOT a probe:
// an external provider outage must not pull a healthy gateway out of rotation.
type ReadinessProbe interface {
	Ping(ctx context.Context) error
}

// New creates a new HTTP server
func New(provider core.RoutableProvider, cfg *Config) *Server {
	e := echo.New()
	e.Logger = slog.Default()
	basePath := configuredBasePath(cfg)
	if basePath != "/" {
		e.Pre(stripBasePathMiddleware(basePath))
	}
	// Keep client IP handling explicit after Echo v5.1.0 changed RealIP defaults.
	// Direct extraction is the safe baseline unless a caller opts into trusted
	// proxy header handling via Config.IPExtractor.
	e.IPExtractor = echo.ExtractIPDirect()
	if cfg != nil && cfg.IPExtractor != nil {
		e.IPExtractor = cfg.IPExtractor
	}

	// Get loggers from config (may be nil)
	var auditLogger auditlog.LoggerInterface
	var usageLogger usage.LoggerInterface
	var budgetChecker BudgetChecker
	var pricingResolver usage.PricingResolver
	if cfg != nil {
		auditLogger = cfg.AuditLogger
		usageLogger = cfg.UsageLogger
		budgetChecker = cfg.BudgetChecker
		pricingResolver = cfg.PricingResolver
	}

	var modelResolver RequestModelResolver
	var modelAuthorizer RequestModelAuthorizer
	var workflowPolicyResolver RequestWorkflowPolicyResolver
	var failoverResolver RequestFailoverResolver
	var translatedRequestPatcher TranslatedRequestPatcher
	if cfg != nil {
		modelResolver = cfg.ModelResolver
		modelAuthorizer = cfg.ModelAuthorizer
		workflowPolicyResolver = cfg.WorkflowPolicyResolver
		failoverResolver = cfg.FailoverResolver
		translatedRequestPatcher = cfg.TranslatedRequestPatcher
	}

	handler := newHandlerWithAuthorizer(provider, auditLogger, usageLogger, pricingResolver, modelResolver, modelAuthorizer, workflowPolicyResolver, failoverResolver, translatedRequestPatcher)
	handler.budgetChecker = budgetChecker
	if cfg != nil {
		handler.batchRequestPreparer = cfg.BatchRequestPreparer
		handler.exposedModelLister = cfg.ExposedModelLister
		handler.keepOnlyAliasesAtModelsEndpoint = cfg.KeepOnlyAliasesAtModelsEndpoint
		handler.responseCache = cfg.ResponseCacheMiddleware
		handler.guardrailsHash = cfg.GuardrailsHash
		handler.storageProbe = cfg.StorageProbe
		handler.cacheProbe = cfg.CacheProbe
	}
	if cfg != nil && cfg.EnabledPassthroughProviders != nil {
		handler.setEnabledPassthroughProviders(cfg.EnabledPassthroughProviders)
	}
	// Mirror the route-registration default below: a nil config enables realtime
	// so the documented default and the registered route stay consistent.
	handler.realtimeEnabled = cfg == nil || cfg.RealtimeEnabled
	if cfg != nil && !passthroughV1PrefixNormalizationEnabled(cfg) {
		handler.normalizePassthroughV1Prefix = false
	}
	if cfg != nil && cfg.BatchStore != nil {
		handler.SetBatchStore(cfg.BatchStore)
	}
	if cfg != nil && cfg.FileStore != nil {
		handler.SetFileStore(cfg.FileStore)
	}
	if cfg != nil && cfg.ResponseStore != nil {
		handler.SetResponseStore(cfg.ResponseStore)
	}
	if cfg != nil && cfg.ConversationStore != nil {
		handler.SetConversationStore(cfg.ConversationStore)
	}

	// Build list of paths that skip authentication
	authSkipPaths := []string{"/health", "/health/ready"}

	// Determine metrics path
	metricsPath := "/metrics"
	if cfg != nil && cfg.MetricsEnabled {
		if cfg.MetricsEndpoint != "" {
			// Normalize path to prevent traversal attacks
			metricsPath = path.Clean(cfg.MetricsEndpoint)
		}
		// Prevent metrics endpoint from shadowing API routes (security: auth bypass)
		if metricsPath == "/v1" || strings.HasPrefix(metricsPath, "/v1/") ||
			metricsPath == "/p" || strings.HasPrefix(metricsPath, "/p/") {
			slog.Warn("metrics endpoint conflicts with API routes, using /metrics instead",
				"configured", cfg.MetricsEndpoint,
				"normalized", metricsPath)
			metricsPath = "/metrics"
		}
		authSkipPaths = append(authSkipPaths, metricsPath)
	}

	// Admin dashboard pages and static assets skip auth (/* enables prefix matching)
	if cfg != nil && cfg.AdminUIEnabled && cfg.DashboardHandler != nil {
		authSkipPaths = append(authSkipPaths, "/admin/dashboard", "/admin/dashboard/*", "/admin/static/*")
	}
	if cfg != nil && cfg.SwaggerEnabled && SwaggerAvailable() {
		authSkipPaths = append(authSkipPaths, "/swagger/*")
	}
	if cfg != nil && cfg.PprofEnabled {
		authSkipPaths = append(authSkipPaths, "/debug/pprof", "/debug/pprof/*")
	}
	if cfg != nil {
		authSkipPaths = append(authSkipPaths, cfg.ExtraAuthSkipPaths...)
	}

	// Global middleware stack (order matters)
	// Request logger with optional filtering for model-only interactions
	if cfg != nil && cfg.LogOnlyModelInteractions {
		e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
			Skipper: func(c *echo.Context) bool {
				return !core.IsModelInteractionPath(c.Request().URL.Path)
			},
			LogStatus:        true,
			LogURI:           true,
			LogMethod:        true,
			LogLatency:       true,
			LogProtocol:      true,
			LogRemoteIP:      true,
			LogHost:          true,
			LogURIPath:       true,
			LogUserAgent:     true,
			LogRequestID:     true,
			LogContentLength: true,
			LogResponseSize:  true,
			LogValuesFunc: func(c *echo.Context, v middleware.RequestLoggerValues) error {
				slog.Info("REQUEST",
					"method", v.Method,
					"uri", v.URI,
					"status", v.Status,
					"latency", v.Latency.String(),
					"host", v.Host,
					"bytes_in", v.ContentLength,
					"bytes_out", v.ResponseSize,
					"user_agent", v.UserAgent,
					"remote_ip", v.RemoteIP,
					"request_id", v.RequestID,
				)
				return nil
			},
		}))
	} else {
		e.Use(middleware.RequestLogger())
	}
	e.Use(middleware.Recover())

	// Body size limit (default: 10MB)
	bodySizeLimit := "10M"
	if cfg != nil && cfg.BodySizeLimit != "" {
		bodySizeLimit = cfg.BodySizeLimit
	}
	e.Use(middleware.BodyLimit(parseBodySizeLimitBytes(bodySizeLimit)))

	// Request ID middleware (always active — ensures every request has a unique ID
	// for usage tracking, audit logging, and response correlation)
	e.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			req, id := ensureRequestID(c.Request())
			c.SetRequest(req)
			c.Response().Header().Set("X-Request-ID", id)
			return next(c)
		}
	})
	e.Use(modelInteractionWriteDeadlineMiddleware())

	// Tenant resolution runs before ingress capture so downstream stages
	// (snapshot, auth, workflow) can read tenantID from context.
	if cfg != nil && cfg.Tenants != nil && cfg.BaseDomain != "" {
		e.Use(TenantResolver(cfg.Tenants, cfg.BaseDomain, cfg.PlatformHost))
	}

	// Ingress capture (before auth/audit/model validation so they can consume shared raw request state)
	userPathHeaderName := configuredUserPathHeader(cfg)
	e.Use(RequestSnapshotCapture(userPathHeaderName))

	// Request labelling from configured tagging headers (after snapshot capture so
	// audit logging still sees the original headers, before audit logging so
	// entries can record the labels)
	if cfg != nil && cfg.Tagging != nil {
		e.Use(TaggingCapture(cfg.Tagging))
	}

	if cfg != nil && len(cfg.PassthroughSemanticEnrichers) > 0 {
		e.Use(PassthroughSemanticEnrichment(provider, cfg.PassthroughSemanticEnrichers, passthroughV1PrefixNormalizationEnabled(cfg)))
	}

	// Audit logging runs before workflow resolution so early workflow resolution/validation
	// failures are still logged. The middleware defers request capture and
	// dynamically gates response capture on the final resolved workflow, so
	// Audit=false still suppresses per-request capture work.
	if cfg != nil && cfg.AuditLogger != nil && cfg.AuditLogger.Config().Enabled {
		e.Use(auditlog.Middleware(cfg.AuditLogger))
	}

	// Extension middleware runs after audit capture and before gateway auth so
	// extensions (e.g. SSO sessions) can normalize credentials for the auth check.
	if cfg != nil {
		for _, m := range cfg.ExtraMiddleware {
			e.Use(m)
		}
	}

	// Authentication (skips public paths)
	if cfg != nil && (cfg.MasterKey != "" || cfg.Authenticator != nil) {
		e.Use(AuthMiddlewareWithAuthenticator(cfg.MasterKey, cfg.Authenticator, authSkipPaths, userPathHeaderName))
	}

	// Request rewriters run post-auth (rewriters only see authenticated
	// traffic) and pre-workflow-resolution (body rewrites, including "model",
	// affect routing, failover, guardrails, budgets, and caching). Not
	// registered when no rewriters exist, so the default build pays nothing.
	if cfg != nil && len(cfg.RequestRewriters) > 0 {
		e.Use(RequestRewriteMiddleware(cfg.RequestRewriters, auditLogger))
	}

	// Workflow resolution resolves the request-scoped workflow after auth so
	// managed auth key user-path overrides are visible to policy resolution while
	// still keeping workflow resolution failures loggable through the audit middleware.
	e.Use(WorkflowResolutionWithResolverAndPolicy(provider, modelResolver, workflowPolicyResolver))

	// Public routes
	e.GET("/health", handler.Health)
	e.GET("/health/ready", handler.Ready)
	registerSwagger(e, cfg)
	if cfg != nil && cfg.MetricsEnabled {
		e.GET(metricsPath, echo.WrapHandler(promhttp.Handler()))
	}
	if cfg != nil && cfg.PprofEnabled {
		e.GET("/debug/pprof", echo.WrapHandler(http.HandlerFunc(httppprof.Index)))
		e.GET("/debug/pprof/", echo.WrapHandler(http.HandlerFunc(httppprof.Index)))
		e.GET("/debug/pprof/cmdline", echo.WrapHandler(http.HandlerFunc(httppprof.Cmdline)))
		e.GET("/debug/pprof/profile", echo.WrapHandler(http.HandlerFunc(httppprof.Profile)))
		e.GET("/debug/pprof/symbol", echo.WrapHandler(http.HandlerFunc(httppprof.Symbol)))
		e.GET("/debug/pprof/trace", echo.WrapHandler(http.HandlerFunc(httppprof.Trace)))
		e.GET("/debug/pprof/:profile", func(c *echo.Context) error {
			httppprof.Handler(c.Param("profile")).ServeHTTP(c.Response(), c.Request())
			return nil
		})
	}

	// API routes
	if cfg == nil || !cfg.DisablePassthroughRoutes {
		e.GET("/p/:provider/*", handler.ProviderPassthrough)
		e.POST("/p/:provider/*", handler.ProviderPassthrough)
		e.PUT("/p/:provider/*", handler.ProviderPassthrough)
		e.PATCH("/p/:provider/*", handler.ProviderPassthrough)
		e.DELETE("/p/:provider/*", handler.ProviderPassthrough)
		e.HEAD("/p/:provider/*", handler.ProviderPassthrough)
		e.OPTIONS("/p/:provider/*", handler.ProviderPassthrough)
	}
	// Inference /v1/* routes live behind hostGuard("tenant"): the inference
	// surface is only reachable on tenant hosts and 404s on the platform host
	// (mirroring the admin plane's hostGuard("platform")). In dev mode (no
	// base_domain) isPlatformHost is always false, so hostGuard passes and the
	// routes stay reachable.
	v1 := e.Group("/v1", hostGuard("tenant"))
	v1.GET("/models", handler.ListModels)
	v1.POST("/chat/completions", handler.ChatCompletion)
	v1.POST("/messages", handler.Messages)
	v1.POST("/messages/count_tokens", handler.CountMessageTokens)
	v1.POST("/responses/input_tokens", handler.ResponseInputTokens)
	v1.POST("/responses/compact", handler.CompactResponse)
	v1.GET("/responses/:id/input_items", handler.ListResponseInputItems)
	v1.POST("/responses/:id/cancel", handler.CancelResponse)
	v1.GET("/responses/:id", handler.GetResponse)
	v1.DELETE("/responses/:id", handler.DeleteResponse)
	v1.POST("/responses", handler.Responses)
	v1.POST("/conversations", handler.CreateConversation)
	v1.GET("/conversations/:id", handler.GetConversation)
	v1.POST("/conversations/:id", handler.UpdateConversation)
	v1.DELETE("/conversations/:id", handler.DeleteConversation)
	v1.POST("/embeddings", handler.Embeddings)
	v1.POST("/audio/speech", handler.AudioSpeech)
	v1.POST("/audio/transcriptions", handler.AudioTranscriptions)
	if cfg == nil || cfg.RealtimeEnabled {
		v1.GET("/realtime", handler.Realtime)
	}
	v1.POST("/files", handler.CreateFile)
	v1.GET("/files", handler.ListFiles)
	v1.GET("/files/:id", handler.GetFile)
	v1.DELETE("/files/:id", handler.DeleteFile)
	v1.GET("/files/:id/content", handler.GetFileContent)
	v1.POST("/batches", handler.Batches)
	v1.GET("/batches", handler.ListBatches)
	v1.GET("/batches/:id", handler.GetBatch)
	v1.POST("/batches/:id/cancel", handler.CancelBatch)
	v1.GET("/batches/:id/results", handler.BatchResults)

	// Admin API routes (behind ADMIN_ENDPOINTS_ENABLED flag). The platform and
	// tenant admin handlers share the /admin prefix. Routes owned by one host
	// kind carry a hostGuard for that kind so the other host sees a 404; paths
	// served by both handlers are registered exactly once and dispatched on the
	// request's host kind (see mountAdminRoutesByHost) — the echo router
	// rejects duplicate method+path registrations, so the two handlers cannot
	// each register the same path twice.
	if cfg != nil && cfg.AdminEndpointsEnabled && (cfg.PlatformAdminHandler != nil || cfg.TenantAdminHandler != nil) {
		// Host resolution only happens when both the tenant service and
		// base_domain are configured (see the TenantResolver middleware above).
		// Without either, mounting host-aware/guarded routes would 404 or scope
		// everything to the "" tenant, so we fall back to the legacy
		// single-tenant surface (full platform surface, no hostGuard).
		baseDomainConfigured := cfg.BaseDomain != "" && cfg.Tenants != nil
		mountAdminRoutesByHost(e.Group("/admin"), cfg.PlatformAdminHandler, cfg.TenantAdminHandler, baseDomainConfigured)

		// Legacy alias under /admin/api/v1/* — accepted until adminLegacySunset
		// to give operators a window to migrate. Responses carry Deprecation,
		// Sunset, and Link headers per RFC 8594 / draft-ietf-httpapi-deprecation-header.
		// The legacy alias predates tenant hosts, so it stays platform-only.
		if cfg.PlatformAdminHandler != nil {
			legacy := e.Group("/admin/api/v1", adminLegacyDeprecationMiddleware)
			if baseDomainConfigured {
				// hostGuard would 404 everything in legacy mode (no host
				// resolution), so it is only attached when hosts are real.
				legacy = e.Group("/admin/api/v1", adminLegacyDeprecationMiddleware, hostGuard("platform"))
			}
			cfg.PlatformAdminHandler.RegisterRoutes(legacy)
			if cfg.PlatformAdminHandler.Default != nil {
				// DashboardConfig moved within /admin from /dashboard/config to
				// /runtime/config; preserve the historical legacy path explicitly.
				legacy.GET("/dashboard/config", cfg.PlatformAdminHandler.Default.DashboardConfig)
			}
		}
	}

	// Admin dashboard UI routes (behind ADMIN_UI_ENABLED flag)
	if cfg != nil && cfg.AdminUIEnabled && cfg.DashboardHandler != nil {
		e.GET("/admin/dashboard", cfg.DashboardHandler.Index)
		e.GET("/admin/dashboard/*", cfg.DashboardHandler.Index)
		e.GET("/admin/static/*", cfg.DashboardHandler.Static)
	}

	// Extension routes register after all core routes.
	if cfg != nil {
		for _, register := range cfg.ExtraRoutes {
			register(e)
		}
	}

	var rcm *responsecache.ResponseCacheMiddleware
	if cfg != nil {
		rcm = cfg.ResponseCacheMiddleware
	}
	return &Server{
		echo:                    e,
		handler:                 handler,
		responseCacheMiddleware: rcm,
		responseStore:           handler.currentResponseStore(),
		conversationStore:       handler.conversationStore,
	}
}

// adminLegacySunset is the sunset date advertised on responses served from the
// deprecated /admin/api/v1/* alias. Format follows RFC 7231 HTTP-date.
const adminLegacySunset = "Sun, 09 Aug 2026 00:00:00 GMT"

// adminLegacyDeprecationMiddleware tags responses on the legacy /admin/api/v1/*
// alias with deprecation signals so clients can detect the move to /admin/*.
func adminLegacyDeprecationMiddleware(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		h := c.Response().Header()
		h.Set("Deprecation", "true")
		h.Set("Sunset", adminLegacySunset)
		h.Set("Link", `</admin/>; rel="successor-version"`)
		return next(c)
	}
}

func passthroughV1PrefixNormalizationEnabled(cfg *Config) bool {
	if cfg == nil || cfg.AllowPassthroughV1Alias == nil {
		return true
	}
	return *cfg.AllowPassthroughV1Alias
}

// Start starts the HTTP server on the given address and exits when ctx is canceled.
func (s *Server) Start(ctx context.Context, addr string) error {
	return newGatewayStartConfig(addr).Start(ctx, s.echo)
}

// StartWithListener starts the HTTP server using a pre-bound listener.
// This is useful in tests that need an already-reserved loopback port.
func (s *Server) StartWithListener(ctx context.Context, listener net.Listener) error {
	sc := echo.StartConfig{
		HideBanner: true,
		Listener:   listener,
	}
	return sc.Start(ctx, s.echo)
}

// Shutdown releases server resources. The HTTP server itself is stopped by
// cancelling the context passed to Start; this method drains any in-flight
// response cache writes, closes the cache store, and closes the response and
// conversation stores.
func (s *Server) Shutdown(_ context.Context) error {
	var firstErr error
	if s.responseCacheMiddleware != nil {
		if err := s.responseCacheMiddleware.Close(); err != nil {
			firstErr = err
		}
	}
	if s.responseStore != nil {
		if err := s.responseStore.Close(); err != nil {
			if firstErr == nil {
				firstErr = err
			} else {
				slog.Warn("response store close failed during shutdown", "error", err)
			}
		}
	}
	if s.conversationStore != nil {
		if err := s.conversationStore.Close(); err != nil {
			if firstErr == nil {
				firstErr = err
			} else {
				slog.Warn("conversation store close failed during shutdown", "error", err)
			}
		}
	}
	return firstErr
}

// ServeHTTP implements the http.Handler interface, allowing Server to be used with httptest
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.echo.ServeHTTP(w, r)
}

func newGatewayStartConfig(addr string) echo.StartConfig {
	return echo.StartConfig{
		Address:    addr,
		HideBanner: true,
		BeforeServeFunc: func(server *http.Server) error {
			return configureGatewayHTTPServer(server)
		},
	}
}

func configureGatewayHTTPServer(server *http.Server) error {
	if server == nil {
		return nil
	}

	// Keep an explicit server-wide write timeout for ordinary routes. Long-lived
	// model interaction routes clear it per request before provider work begins.
	server.ReadTimeout = inboundServerReadTimeout
	server.ReadHeaderTimeout = inboundServerReadHeaderTimeout
	server.WriteTimeout = inboundServerWriteTimeout
	return nil
}

func modelInteractionWriteDeadlineMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if !core.IsModelInteractionPath(c.Request().URL.Path) {
				return next(c)
			}
			if err := http.NewResponseController(c.Response()).SetWriteDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
				slog.Warn("failed to clear write deadline for model interaction",
					"path", c.Request().URL.Path,
					"request_id", requestIDFromContextOrHeader(c.Request()),
					"error", err,
				)
			}
			return next(c)
		}
	}
}

func parseBodySizeLimitBytes(limit string) int64 {
	limit = strings.TrimSpace(limit)
	if limit == "" {
		return config.DefaultBodySizeLimit
	}

	value, err := config.ParseBodySizeLimitBytes(limit)
	if err != nil {
		slog.Warn("invalid body size limit, falling back to default", "configured", limit)
		return config.DefaultBodySizeLimit
	}

	return value
}
