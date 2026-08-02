package auditlog

import (
	"strings"
	"testing"
	"time"
)

func TestBuildAuditLogInsert(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()

	query, args := buildAuditLogInsert("", []*LogEntry{
		{
			ID:             "log-1",
			Timestamp:      now,
			DurationNs:     1234,
			RequestedModel: "gpt-4o-mini",
			ResolvedModel:  "gpt-4o-mini",
			Provider:       "openai",
			ProviderName:   "primary-openai",
			AliasUsed:      true,
			CacheType:      CacheTypeExact,
			StatusCode:     200,
			RequestID:      "req-1",
			AuthKeyID:      "auth-key-1",
			ClientIP:       "127.0.0.1",
			Method:         "POST",
			Path:           "/v1/chat/completions",
			UserPath:       "/team/alpha",
			Stream:         true,
			ErrorType:      "",
			Data: &LogData{
				UserAgent: "test-agent",
			},
		},
		{
			ID:             "log-2",
			Timestamp:      now.Add(time.Second),
			DurationNs:     5678,
			RequestedModel: "gpt-4.1",
			ResolvedModel:  "gpt-4.1",
			Provider:       "openai",
			AliasUsed:      false,
			StatusCode:     500,
			RequestID:      "req-2",
			ClientIP:       "10.0.0.1",
			Method:         "POST",
			Path:           "/v1/responses",
			Stream:         false,
			ErrorType:      "server_error",
			Data:           nil,
		},
	})

	normalized := strings.Join(strings.Fields(query), " ")
	wantQuery := "INSERT INTO audit_logs (id, timestamp, duration_ns, tenant_id, requested_model, resolved_model, provider, provider_name, alias_used, workflow_version_id, cache_type, status_code, request_id, auth_key_id, auth_method, client_ip, method, path, user_path, stream, error_type, data) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22), ($23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44) ON CONFLICT (id) DO NOTHING"
	if normalized != wantQuery {
		t.Fatalf("query = %q, want %q", normalized, wantQuery)
	}

	if got, want := len(args), 44; got != want {
		t.Fatalf("len(args) = %d, want %d", got, want)
	}
	if got := args[0]; got != "log-1" {
		t.Fatalf("args[0] = %v, want log-1", got)
	}
	if got := args[3]; got != "" {
		t.Fatalf("args[3] = %v, want \"\"", got)
	}
	if got := args[7]; got != "primary-openai" {
		t.Fatalf("args[7] = %v, want primary-openai", got)
	}
	if got := args[10]; got != CacheTypeExact {
		t.Fatalf("args[10] = %v, want %q", got, CacheTypeExact)
	}
	if got, ok := args[13].(string); !ok || got != "auth-key-1" {
		t.Fatalf("args[13] = (%T) %v, want (string) auth-key-1", args[13], args[13])
	}
	if got, ok := args[14].(string); !ok || got != "" {
		t.Fatalf("args[14] = (%T) %v, want (string) \"\"", args[14], args[14])
	}
	if got, ok := args[17].(string); !ok || got != "/v1/chat/completions" {
		t.Fatalf("args[17] = (%T) %v, want (string) /v1/chat/completions", args[17], args[17])
	}
	if got, ok := args[18].(string); !ok || got != "/team/alpha" {
		t.Fatalf("args[18] = (%T) %v, want (string) /team/alpha", args[18], args[18])
	}
	if got := string(args[21].([]byte)); got != `{"user_agent":"test-agent"}` {
		t.Fatalf("args[21] = %q, want %q", got, `{"user_agent":"test-agent"}`)
	}
	if got := args[22]; got != "log-2" {
		t.Fatalf("args[22] = %v, want log-2", got)
	}
	if got, ok := args[35].(string); !ok || got != "" {
		t.Fatalf("args[35] = (%T) %v, want (string) \"\"", args[35], args[35])
	}
	if got, ok := args[36].(string); !ok || got != "" {
		t.Fatalf("args[36] = (%T) %v, want (string) \"\"", args[36], args[36])
	}
	if got := args[32]; got != nil {
		t.Fatalf("args[31] = %v, want nil cache type", got)
	}
	if got, ok := args[40].(string); !ok || got != "/" {
		t.Fatalf("args[40] = (%T) %v, want (string) \"/\"", args[40], args[40])
	}
	dataJSON, ok := args[43].([]byte)
	if !ok {
		t.Fatalf("args[43] has type %T, want []byte", args[43])
	}
	if dataJSON != nil {
		t.Fatalf("args[43] = %v, want nil data", dataJSON)
	}
}

func TestAuditLogInsertMaxRowsPerQueryRespectsPostgresLimit(t *testing.T) {
	if got := auditLogInsertMaxRowsPerQuery * auditLogInsertColumnCount; got > postgresMaxBindParameters {
		t.Fatalf("bind parameters = %d, want <= %d", got, postgresMaxBindParameters)
	}
}
