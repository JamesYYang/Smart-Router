# CODEBUDDY.md

This file provides guidance to CodeBuddy Code when working with code in this repository.

## What this is

SmartRouter (Go module `smartrouter`) is an OpenAI-compatible LLM gateway that routes
chat/embeddings/batch/realtime requests across many providers (OpenAI, Anthropic, Gemini/Vertex,
Bedrock, Azure, xAI, Groq, Fireworks, OpenRouter, DeepSeek, Z.ai, MiniMax, Xiaomi MiMo, Bailian,
Oracle, OpenCode Zen, Ollama, vLLM, and arbitrary OpenAI-compatible endpoints). It was bootstrapped
from [GoModel](https://github.com/ENTERPILOT/GoModel) (see `NOTICE.md`) and rebranded — the
binary/module/directories are `smartrouter`, not `gomodel`; any `gomodel` / `cmd/gomodel` reference
is stale and should be treated as such.

The product direction (`docs/design/Smart-Router.md`, in Chinese) is to become an aggregation layer
for Chinese-model providers with **task-level smart routing** — auto-classifying a request's task type
and routing it to the cheapest adequate model — layered on top of the existing provider/cost/failover
infrastructure. The P0/MVP version is implemented in `internal/taskrouting` (rule-based classifier;
off by default) and mounted via the `ModelResolver` field on the gateway (see
`internal/gateway/request_model_resolution.go`'s `ResolveExecutionSelector`). It reuses
`internal/virtualmodels`' existing multi-target + `round_robin`/`cost` execution layer rather than
reimplementing load balancing/failover. `docs/design/TaskRouting-Design.md` documents the mounting
points; read it before extending task routing.

## Build, test, run

- Build: `go build ./...`
- Build with Swagger UI/docs (gated by build tag): `go build -tags=swagger ./...`
  - `internal/server` and `cmd/smartrouter` each have `swagger_enabled.go`/`swagger_disabled.go`
    pairs gated by the `swagger` build tag.
- Run: `go run ./cmd/smartrouter` (no config file required; defaults to `:8080`). Needs at least one
  provider API key set, e.g. `OPENAI_API_KEY=sk-...`.
- CLI flags: `--version`, `--health` (liveness check then exit), `--ready` (readiness check then exit).
- Run all Go tests: `go test ./...`
- Run one package's tests: `go test ./internal/gateway/...`
- Run a single test: `go test ./internal/gateway/... -run TestName -v`
- Dashboard JS tests (no package.json/npm — run directly with Node's built-in test runner):
  `node --test internal/admin/dashboard/static/js/modules/*.test.cjs`, or a single file:
  `node --test internal/admin/dashboard/static/js/modules/pricing.test.cjs`. These tests load the
  corresponding `.js` module source into a `vm` context and exercise it directly (no bundler/DOM).
- No Makefile or CI workflow in this repo; use `go build`/`go test`/`go vet` directly.

## Architecture

Full request-flow diagram: `docs/design/GoModel-Architecture.md`. Design/decision docs live in
`docs/design/` (written in Chinese) — read them before large architectural changes, especially when
touching provider routing, virtual models, or workflow/guardrails coupling.

Request flow for `POST /v1/chat/completions`:
1. **Middleware chain** (`internal/server/http.go`, fixed order): request logging → recover → body
   limit → request ID → write deadline → request snapshot capture → tagging capture → passthrough
   semantic enrichment → audit log → extension middleware → auth → request rewrite → workflow
   resolution.
2. **Handler layer** (`internal/server`, e.g. `translated_inference_service.go`): parses the request,
   calls `gateway.PrepareChatRequest` to resolve the model selector and workflow, then `enforceBudget`,
   then `handleWithCache`. **Budget is checked before the cache lookup** — a request served from cache
   still passes budget enforcement.
3. **Inference orchestration** (`internal/gateway/inference_execute.go`): resolves the provider via
   `internal/providers.Router`, resolves virtual-model targets via `internal/virtualmodels` (aliasing /
   load-balancing with `round_robin` or `cost` strategy) if the selector targets a virtual model, then
   calls the provider and applies `internal/failover` retry/circuit-breaking on failure. Task routing
   (if enabled) rewrites the requested model earlier in model resolution
   (`internal/gateway/request_model_resolution.go` → `ResolveExecutionSelector`) before this stage.
4. **Response post-processing**: normalize to OpenAI-compatible format, run guardrails on the response
   (if enabled), log usage/cost via `internal/usage` (pricing resolved via `internal/pricingoverrides`),
   write to `internal/responsecache` if enabled, flush audit log / live dashboard events.

Streaming requests share the same prepare/budget stages; requests with no request rewriting and no
forced usage reporting take a "fast path" that proxies the upstream SSE stream directly.

Key modules:
- `internal/core` — shared domain types (`ChatRequest`/`Response`, `Model`, `Workflow`, `Selector`)
  and protocol codecs used by nearly every other package.
- `internal/gateway` — the `InferenceOrchestrator`: model resolution, dispatch, failover, usage
  logging. `request_model_resolution.go` is the integration point for any model-selection extension
  (task routing, aliases, etc.).
- `internal/server` — HTTP handlers, routing, middleware wiring on top of `gateway`.
- `internal/providers` — provider registry + `Router` (selector → provider adapter); one subpackage
  per provider (`internal/providers/<name>/`); `factory.go`/`registry.go`/`registry_cache.go` (model
  discovery cache) and `router.go` (selector → provider dispatch) glue them together.
- `internal/app` — assembly layer (`app.New`): wires every module together per loaded config and owns
  component lifecycle/shutdown ordering (`New` unwinds already-initialized components on failure).
- `internal/virtualmodels` — alias/load-balancing redirect table (`round_robin`/`cost` strategies); the
  execution layer that task-level routing reuses rather than reimplementing.
- `internal/taskrouting` — P0/MVP task-level smart routing: rule-based classifier
  (`agent_tool_use`/`code`/`translation`/`title_generation`/`bulk_low_value`/`copywriting`/
  `data_extraction`/`general`) that rewrites an entrypoint virtual model to a task-specific virtual
  model; implements the gateway `ModelResolver` interface; default-off.
- `internal/workflows` — compiles guardrails/rewrite rules into a `core.Workflow` per request; a
  default global workflow is always created at startup (it cannot be fully disabled even if unused),
  so guardrails/workflow logic should be treated as coupled when changing either.
- `internal/budget`, `internal/usage`, `internal/pricingoverrides` — budget enforcement, usage/cost
  recording, and manual price overrides, all keyed off the same selector/pricing data.
- `internal/failover` — retry/circuit-breaking, applied at the provider-call layer (composes with, but
  is distinct from, virtual-model load balancing which happens one layer up).
- `internal/guardrails`, `internal/responsecache`, `internal/batch`, `internal/conversationstore`,
  `internal/tagging`, `internal/auditlog` — optional pipeline stages hung off the main chain via
  middleware or prepare-stage hooks; most are individually feature-flagged (see env vars in
  `config/config.example.yaml`), except `tagging`, `workflows`, `batch`, `conversationstore`, and
  `embedding`, which always initialize and register routes regardless of use.
- `run/` — CLI entry point shared by all binaries (`run.Run`); handles flag parsing, `--version`,
  `--health`/`--ready` probe modes, dotenv loading, logging setup, config loading, graceful shutdown
  on SIGINT/SIGTERM.
- `ext/` — extension registry (`ext.Registry`) letting external modules register request rewriters,
  middleware, and routes into `run.Run` without modifying core packages (see `run/run.go` package doc
  for the pattern of building a custom gateway binary around this package).
- `internal/admin` — Admin REST API (`/admin/...`) and dashboard UI (`internal/admin/dashboard`, HTML
  templates + vanilla JS modules under `static/js/modules`, each with a co-located `.test.cjs`).

## Conventions

- One `_test.go` file per source file, same package, same base name (e.g. `router.go` /
  `router_test.go`); uses `testify` (`require`/`assert`).
- Each provider lives in its own `internal/providers/<name>` subpackage implementing the common
  provider adapter interface; add new providers there rather than branching in shared files.
- Config is loaded once via `config.Load()` into `*config.Config`; env vars always override
  `config/config.yaml` values (see comments in `config.example.yaml` for the exact env var name per
  setting, including array-like env vars such as `TAGGING_HEADER_1`, `VIRTUAL_MODELS`).
- Feature areas are individually toggleable by config/env flags; check `config.example.yaml` before
  assuming a module is always active.
- Dashboard JS modules (`internal/admin/dashboard/static/js/modules/*.js`) attach factories to
  `window.dashboard*Module` so they're loadable both in-browser and in Node's `vm` context for
  testing; keep new modules to this pattern if they need `.test.cjs` coverage.
- Models are addressed as `provider/model-id` (e.g. `openai/gpt-4o-mini`,
  `anthropic/claude-sonnet-4-6`). Provider model lists are discovered dynamically via each provider's
  `/models` endpoint by default; pricing metadata comes from the
  [ai-model-list](https://github.com/ENTERPILOT/ai-model-list) registry. `models.configured_provider_models_mode`
  (`fallback` or `allowlist`) controls whether configured `models` lists supplement or restrict
  discovery.
