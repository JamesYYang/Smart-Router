# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

SmartRouter (Go module `smartrouter`) is an OpenAI-compatible LLM gateway that routes chat/embeddings/batch
requests across many providers (OpenAI, Anthropic, Gemini/Vertex, Bedrock, Azure, Groq, Fireworks,
OpenRouter, DeepSeek, Z.ai, MiniMax, Xiaomi MiMo, Bailian, Oracle, Ollama, vLLM, and arbitrary
OpenAI-compatible endpoints). It was bootstrapped from [GoModel](https://github.com/ENTERPILOT/GoModel)
(see `NOTICE.md`) and rebranded — the binary/module/directories are `smartrouter`, not `gomodel`; if you
see `gomodel` referenced anywhere, treat it as stale.

The product direction (`docs/design/Smart-Router.md`, in Chinese) is to become an aggregation layer
specifically for Chinese-model providers with **task-level smart routing** — auto-classifying a request's
task type and routing it to the cheapest adequate model — layered on top of the existing
provider/cost/failover infrastructure. `docs/design/TaskRouting-Design.md` documents the planned mounting
points (`internal/gateway/request_model_resolution.go`'s `ResolveExecutionSelector`) for that feature
before it's built: the intent is to reuse `virtualmodels`' existing multi-target + `round_robin`/`cost`
strategy execution layer rather than reimplementing load balancing/failover/availability probing.

## Build, test, run

- Build: `go build ./...`
- Build with Swagger UI/docs (gated by build tag): `go build -tags=swagger ./...`
  - `internal/server` and `cmd/smartrouter` each have `swagger_enabled.go`/`swagger_disabled.go` pairs
    gated by the `swagger` build tag.
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

Full request-flow diagram: `docs/design/GoModel-Architecture.md`.

Request flow for `POST /v1/chat/completions`:
1. **Middleware chain** (`internal/server/http.go`, fixed order): request logging → recover → body limit
   → request ID → write deadline → request snapshot capture → tagging capture → passthrough semantic
   enrichment → audit log → extension middleware → auth → request rewrite → workflow resolution.
2. **Handler layer** (`internal/server`, e.g. `translated_inference_service.go`): parses the request,
   calls `gateway.PrepareChatRequest` to resolve the model selector and workflow, then `enforceBudget`,
   then `handleWithCache`. **Budget is checked before the cache lookup** — a request served from cache
   still passes budget enforcement.
3. **Inference orchestration** (`internal/gateway/inference_execute.go`): resolves the provider via
   `internal/providers.Router`, resolves virtual-model targets via `internal/virtualmodels` (aliasing /
   load-balancing with `round_robin` or `cost` strategy) if the selector targets a virtual model, then
   calls the provider and applies `internal/failover` retry/circuit-breaking on failure.
4. **Response post-processing**: normalize to OpenAI-compatible format, run guardrails on the response
   (if enabled), log usage/cost via `internal/usage` (pricing resolved via `internal/pricingoverrides`),
   write to `internal/responsecache` if enabled, flush audit log / live dashboard events.

Streaming requests share the same prepare/budget stages; requests with no request rewriting and no forced
usage reporting take a "fast path" that proxies the upstream SSE stream directly.

Key modules:
- `internal/core` — shared domain types (`ChatRequest`/`Response`, `Model`, `Workflow`, `Selector`) and
  protocol codecs used by nearly every other package.
- `internal/gateway` — the `InferenceOrchestrator`: model resolution, dispatch, failover, usage logging.
- `internal/server` — HTTP handlers, routing, middleware wiring on top of `gateway`.
- `internal/providers` — provider registry + `Router` (selector → provider adapter); one subpackage per
  provider (`internal/providers/<name>/`); `factory.go`/`registry.go`/`registry_cache.go` (model discovery
  cache) and `router.go` (selector → provider dispatch) glue them together.
- `internal/app` — assembly layer (`app.New`): wires every module together per loaded config and owns
  component lifecycle/shutdown ordering (`New` unwinds already-initialized components on failure).
- `internal/virtualmodels` — alias/load-balancing redirect table (`round_robin`/`cost` strategies); the
  existing execution layer that the planned task-level routing feature is meant to reuse rather than
  reimplement (see Design docs above).
- `internal/workflows` — compiles guardrails/rewrite rules into a `core.Workflow` per request; a default
  global workflow is always created at startup (it cannot be fully disabled even if unused), so
  guardrails/workflow logic should be treated as coupled when changing either.
- `internal/budget`, `internal/usage`, `internal/pricingoverrides` — budget enforcement, usage/cost
  recording, and manual price overrides, all keyed off the same selector/pricing data.
- `internal/failover` — retry/circuit-breaking, applied at the provider-call layer (composes with, but is
  distinct from, virtual-model load balancing which happens one layer up).
- `internal/guardrails`, `internal/responsecache`, `internal/batch`, `internal/conversationstore`,
  `internal/tagging`, `internal/auditlog` — optional pipeline stages hung off the main chain via
  middleware or prepare-stage hooks; most are individually feature-flagged (see env vars in
  `config/config.example.yaml`), except `tagging`, `workflows`, `batch`, `conversationstore`, and
  `embedding`, which always initialize and register routes regardless of use.
- `run/` — CLI entry point shared by all binaries (`run.Run`); handles flag parsing, `--version`,
  `--health`/`--ready` probe modes, dotenv loading, logging setup, config loading, graceful shutdown on
  SIGINT/SIGTERM.
- `ext/` — extension registry (`ext.Registry`) letting external modules register request rewriters,
  middleware, and routes into `run.Run` without modifying core packages (see `run/run.go` package doc for
  the pattern of building a custom gateway binary around this package).
- `internal/admin` — Admin REST API (`/admin/...`) and dashboard UI (`internal/admin/dashboard`, HTML
  templates + vanilla JS modules under `static/js/modules`, each with a co-located `.test.cjs`).

## Conventions

- One `_test.go` file per source file, same package, same base name (e.g. `router.go` / `router_test.go`);
  uses `testify` (`require`/`assert`).
- Each provider lives in its own `internal/providers/<name>` subpackage implementing the common provider
  adapter interface; add new providers there rather than branching in shared files.
- Config is loaded once via `config.Load()` into `*config.Config`; env vars always override
  `config/config.yaml` values (see comments in `config.example.yaml` for the exact env var name per
  setting, including array-like env vars such as `TAGGING_HEADER_1`, `VIRTUAL_MODELS`).
- Feature areas are individually toggleable by config/env flags; check `config.example.yaml` before
  assuming a module is always active.
- Design/decision docs live in `docs/design/` (written in Chinese) — read them before large architectural
  changes, especially when touching provider routing, virtual models, or workflow/guardrails coupling.
- Dashboard JS modules (`internal/admin/dashboard/static/js/modules/*.js`) attach factories to
  `window.dashboard*Module` so they're loadable both in-browser and in Node's `vm` context for testing;
  keep new modules to this pattern if they need `.test.cjs` coverage.
