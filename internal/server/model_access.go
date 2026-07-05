package server

import "smartrouter/internal/gateway"

// RequestModelAuthorizer validates request-scoped access to concrete models.
type RequestModelAuthorizer = gateway.ModelAuthorizer
