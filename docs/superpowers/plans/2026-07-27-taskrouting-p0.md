# TaskRouting P0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the P0 (MVP) phase of TaskRouting per `docs/superpowers/specs/2026-07-27-taskrouting-design.md`: a rule-based classifier that rewrites `smart-router/auto`-style virtual model requests to a task-specific virtual model, reusing the existing `virtualmodels` resolution/load-balancing/failover pipeline.

**Architecture:** A new `internal/taskrouting` package implements `gateway.ModelResolver` + `gateway.UserPathModelResolver` + a new optional `gateway.LabelingModelResolver`, wrapping `virtualmodels.Service` at the two places `internal/app/app.go` wires up `ModelResolver`. The classifier reads message/tool-call signals from `core.RequestSnapshot` (already on the request context — see Task 1 note below), not from the `ModelResolver` interface itself, which only ever receives a model-name string.

**Tech Stack:** Go stdlib only in the new package (no new dependencies); `github.com/goccy/go-json` for JSON parsing (the project's `encoding/json` replacement — see Task 4); plain `testing` (not `testify`) to match the prevailing style in `internal/gateway`, `internal/virtualmodels`, and `config`, the packages this work sits alongside.

## Global Constraints

- Go module `smartrouter`; build with `go build ./...`, full test suite with `go test ./...`, both must stay green after every task.
- One `_test.go` file per source file, same package, same base name (project convention).
- `TASK_ROUTING_ENABLED` env var must be added to `clearAllConfigEnvVars` in `config/config_test.go` so config tests keep starting from a clean slate (Task 3).
- Nothing in this plan may change behavior when `task_routing.enabled` is `false` (the default) — every new code path must be provably a no-op in that state.
- Fail-open everywhere at request-serving time: classifier errors, unmapped tasks, and missing request snapshots must never cause a request to fail. Fail-closed only at config-load time (startup), matching the existing `normalizeTaggingConfig` convention.
- Do not modify `internal/virtualmodels`, `internal/failover`, or provider packages — the whole point of the mounting point is to avoid touching them.

---

### Task 1: Gateway — propagate optional resolver labels into `RequestModelResolution`

**Files:**
- Modify: `internal/core/request_model_resolution.go`
- Modify: `internal/gateway/interfaces.go`
- Modify: `internal/gateway/request_model_resolution.go`
- Modify: `internal/gateway/batch_selection.go:99`
- Test: `internal/gateway/request_model_resolution_test.go`

**Why this task exists:** `docs/design/TaskRouting-Design.md` §9 assumed classification results could reach `usage` labels for free. They can't: `gateway.ModelResolver.ResolveModel` returns `(core.ModelSelector, bool, error)` with no slot for extra data, and `context.Context` is passed by value, so a resolver mutating its own `ctx` argument doesn't propagate anywhere. This task adds one new optional interface (mirroring how `UserPathModelResolver` is already optionally detected) so a resolver *can* report labels without changing the contract every other `ModelResolver` implementer (`virtualmodels.Service`, providers) relies on.

**Interfaces:**
- Produces: `core.RequestModelResolution.Labels []string` — populated whenever the active resolver implements the new interface and returns non-empty labels; `nil` otherwise (zero behavior change for existing resolvers).
- Produces: `gateway.LabelingModelResolver` interface with method `ResolveModelWithLabels(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error)`.
- Consumes: nothing from other tasks (this is the foundation task).

- [ ] **Step 1: Add the `Labels` field to `core.RequestModelResolution`**

Open `internal/core/request_model_resolution.go`. It currently reads:

```go
package core

// RequestModelResolution ...
type RequestModelResolution struct {
	Requested        RequestedModelSelector
	ResolvedSelector ModelSelector
	ProviderType     string
	ProviderName     string
	AliasApplied     bool
}
```

Add a `Labels` field:

```go
type RequestModelResolution struct {
	Requested        RequestedModelSelector
	ResolvedSelector ModelSelector
	ProviderType     string
	ProviderName     string
	AliasApplied     bool
	// Labels are extra observability labels a ModelResolver attached during
	// resolution (e.g. "task:code" from TaskRouting classification), merged
	// into the request's context labels by gateway.ensureTranslatedRequestWorkflow.
	// Empty for resolvers that don't implement LabelingModelResolver.
	Labels []string
}
```

- [ ] **Step 2: Run existing core tests to confirm nothing broke**

Run: `go test ./internal/core/... -run TestRequestModelResolution -v`
Expected: PASS (existing `TestRequestModelResolutionRequestedQualifiedModel` cases still pass; adding a field doesn't change any existing behavior).

- [ ] **Step 3: Add the `LabelingModelResolver` interface**

Open `internal/gateway/interfaces.go`. After the `UserPathModelResolver` type (currently ending at line 23), add:

```go
// LabelingModelResolver is an optional ModelResolver that reports extra
// observability labels (e.g. task-classification results) alongside the
// resolved selector, without changing the ModelResolver contract other
// implementers rely on. ResolveExecutionSelector prefers this over
// ResolveModel/ResolveModelForUserPath when a resolver implements it.
type LabelingModelResolver interface {
	ResolveModelWithLabels(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error)
}
```

- [ ] **Step 4: Write failing tests for label propagation in `request_model_resolution_test.go`**

Open `internal/gateway/request_model_resolution_test.go`. After the existing `requestAliasResolver` type (around line 92-100), add a new fake resolver and two tests:

```go
type requestLabelingResolver struct {
	selector core.ModelSelector
	labels   []string
}

func (r requestLabelingResolver) ResolveModel(requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	selector, changed, _, err := r.ResolveModelWithLabels(context.Background(), requested)
	return selector, changed, err
}

func (r requestLabelingResolver) ResolveModelWithLabels(_ context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error) {
	if requested.RequestedQualifiedModel() == "smart-router/auto" {
		return r.selector, true, r.labels, nil
	}
	selector, err := requested.Normalize()
	return selector, false, nil, err
}
```

```go
func TestResolveRequestModelWithAuthorizer_PropagatesLabelingResolverLabels(t *testing.T) {
	provider := newRequestRefreshProvider(1)
	provider.supported["deepseek/deepseek-coder"] = true
	provider.providerType["deepseek/deepseek-coder"] = "deepseek"
	resolver := requestLabelingResolver{
		selector: core.ModelSelector{Provider: "deepseek", Model: "deepseek-coder"},
		labels:   []string{"task:code"},
	}

	resolution, err := ResolveRequestModelWithAuthorizer(
		context.Background(),
		provider,
		resolver,
		nil,
		core.NewRequestedModelSelector("smart-router/auto", ""),
	)
	if err != nil {
		t.Fatalf("ResolveRequestModelWithAuthorizer() error = %v, want nil", err)
	}
	if len(resolution.Labels) != 1 || resolution.Labels[0] != "task:code" {
		t.Fatalf("resolution.Labels = %v, want [task:code]", resolution.Labels)
	}
}

func TestResolveRequestModelWithAuthorizer_NoLabelsWhenResolverIsPlain(t *testing.T) {
	provider := newRequestRefreshProvider(1)

	resolution, err := ResolveRequestModelWithAuthorizer(
		context.Background(),
		provider,
		nil,
		nil,
		core.NewRequestedModelSelector("openai/gpt-4o", ""),
	)
	if err != nil {
		t.Fatalf("ResolveRequestModelWithAuthorizer() error = %v, want nil", err)
	}
	if resolution.Labels != nil {
		t.Fatalf("resolution.Labels = %v, want nil", resolution.Labels)
	}
}
```

- [ ] **Step 5: Run the new tests to verify they fail**

Run: `go test ./internal/gateway/... -run TestResolveRequestModelWithAuthorizer_PropagatesLabelingResolverLabels -v`
Expected: FAIL to compile — `resolveModel` doesn't return a 4th value yet, `ResolveExecutionSelector`'s signature doesn't match, `RequestModelResolution.Labels` assignment is missing. (Step 1 already added the field, so this specifically fails on `ResolveExecutionSelector`/`resolveRequestedModel` signatures not existing yet.)

- [ ] **Step 6: Thread labels through `resolveRequestedModel` and `ResolveExecutionSelector`**

Open `internal/gateway/request_model_resolution.go`. Replace the `resolveRequestedModel` function (currently at the bottom of the file):

```go
// resolveRequestedModel resolves through a UserPathModelResolver when the
// resolver implements it (applying user_path-scoped redirects), falling back to
// the unscoped ModelResolver otherwise.
func resolveRequestedModel(ctx context.Context, resolver ModelResolver, requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	if scoped, ok := resolver.(UserPathModelResolver); ok {
		return scoped.ResolveModelForUserPath(ctx, requested)
	}
	return resolver.ResolveModel(requested)
}
```

with:

```go
// resolveRequestedModel resolves through a LabelingModelResolver when the
// resolver implements it (capturing any observability labels it attaches),
// else through a UserPathModelResolver (applying user_path-scoped redirects),
// falling back to the unscoped ModelResolver otherwise.
func resolveRequestedModel(ctx context.Context, resolver ModelResolver, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error) {
	if labeling, ok := resolver.(LabelingModelResolver); ok {
		return labeling.ResolveModelWithLabels(ctx, requested)
	}
	if scoped, ok := resolver.(UserPathModelResolver); ok {
		selector, changed, err := scoped.ResolveModelForUserPath(ctx, requested)
		return selector, changed, nil, err
	}
	selector, changed, err := resolver.ResolveModel(requested)
	return selector, changed, nil, err
}
```

Then replace `ResolveExecutionSelector` (same file):

```go
// ResolveExecutionSelector applies explicit and provider-owned selector
// resolution. ctx carries the effective request user path so a resolver that
// implements UserPathModelResolver can apply user_path-scoped redirects.
func ResolveExecutionSelector(
	ctx context.Context,
	provider core.RoutableProvider,
	resolver ModelResolver,
	requested core.RequestedModelSelector,
) (core.ModelSelector, bool, []string, error) {
	requested = core.NewRequestedModelSelector(requested.Model, requested.ProviderHint)

	var (
		resolvedSelector core.ModelSelector
		aliasApplied     bool
		labels           []string
		err              error
	)

	if resolver != nil {
		resolvedSelector, aliasApplied, labels, err = resolveRequestedModel(ctx, resolver, requested)
		if err != nil {
			return core.ModelSelector{}, false, nil, err
		}
		requested = core.NewRequestedModelSelector(resolvedSelector.QualifiedModel(), "")
	}

	if providerResolver, ok := provider.(ModelResolver); ok {
		providerSelector, providerChanged, err := providerResolver.ResolveModel(requested)
		if err != nil {
			if resolvedSelector != (core.ModelSelector{}) {
				// Preserve alias targets so callers can refresh the concrete provider before retrying.
				return resolvedSelector, aliasApplied, labels, err
			}
			return core.ModelSelector{}, false, nil, err
		}
		return providerSelector, aliasApplied || providerChanged, labels, nil
	}

	if resolvedSelector != (core.ModelSelector{}) {
		return resolvedSelector, aliasApplied, labels, nil
	}

	resolvedSelector, err = requested.Normalize()
	return resolvedSelector, aliasApplied, labels, err
}
```

- [ ] **Step 7: Update the 4 call sites in `ResolveRequestModelWithAuthorizer` and the return struct**

Same file. Replace the whole `ResolveRequestModelWithAuthorizer` function body:

```go
func ResolveRequestModelWithAuthorizer(
	ctx context.Context,
	provider core.RoutableProvider,
	resolver ModelResolver,
	authorizer ModelAuthorizer,
	requested core.RequestedModelSelector,
) (*core.RequestModelResolution, error) {
	requested = core.NewRequestedModelSelector(requested.Model, requested.ProviderHint)

	resolvedSelector, aliasApplied, labels, err := ResolveExecutionSelector(ctx, provider, resolver, requested)
	refreshed := false
	if err != nil {
		var refreshErr error
		refreshed, refreshErr = refreshProviderModelsForResolution(ctx, provider, resolver, requested, resolvedSelector)
		if refreshErr != nil {
			return nil, refreshErr
		}
		if !refreshed {
			return nil, core.NewInvalidRequestError(err.Error(), err)
		}
		resolvedSelector, aliasApplied, labels, err = ResolveExecutionSelector(ctx, provider, resolver, requested)
		if err != nil {
			return nil, core.NewInvalidRequestError(err.Error(), err)
		}
	}
	if resolvedSelector == (core.ModelSelector{}) {
		resolvedSelector, err = requested.Normalize()
		if err != nil {
			return nil, core.NewInvalidRequestError(err.Error(), err)
		}
	}

	resolvedModel := resolvedSelector.QualifiedModel()
	if counted, ok := provider.(modelCountProvider); ok && counted.ModelCount() == 0 {
		if !refreshed {
			var refreshErr error
			refreshed, refreshErr = refreshProviderModelsForResolution(ctx, provider, resolver, requested, resolvedSelector)
			if refreshErr != nil {
				return nil, refreshErr
			}
			if refreshed {
				resolvedSelector, aliasApplied, labels, err = ResolveExecutionSelector(ctx, provider, resolver, requested)
				if err != nil {
					return nil, core.NewInvalidRequestError(err.Error(), err)
				}
				resolvedModel = resolvedSelector.QualifiedModel()
			}
		}
	}
	if counted, ok := provider.(modelCountProvider); ok && counted.ModelCount() == 0 {
		return nil, core.NewProviderError("", 0, "model registry not initialized", nil)
	}
	if !provider.Supports(resolvedModel) {
		if !refreshed {
			var refreshErr error
			refreshed, refreshErr = refreshProviderModelsForResolution(ctx, provider, resolver, requested, resolvedSelector)
			if refreshErr != nil {
				return nil, refreshErr
			}
			if refreshed {
				resolvedSelector, aliasApplied, labels, err = ResolveExecutionSelector(ctx, provider, resolver, requested)
				if err != nil {
					return nil, core.NewInvalidRequestError(err.Error(), err)
				}
				resolvedModel = resolvedSelector.QualifiedModel()
			}
		}
	}
	if !provider.Supports(resolvedModel) {
		return nil, core.NewModelNotFoundError(resolvedModel)
	}
	if authorizer != nil {
		if err := authorizer.ValidateModelAccess(ctx, resolvedSelector); err != nil {
			return nil, err
		}
	}

	return &core.RequestModelResolution{
		Requested:        requested,
		ResolvedSelector: resolvedSelector,
		ProviderType:     strings.TrimSpace(provider.GetProviderType(resolvedModel)),
		ProviderName:     ResolvedProviderName(provider, resolvedSelector, ""),
		AliasApplied:     aliasApplied,
		Labels:           labels,
	}, nil
}
```

(Only change from the original: `labels` declared/threaded alongside `resolvedSelector`/`aliasApplied` at every `ResolveExecutionSelector` call, and set on the returned struct.)

- [ ] **Step 8: Fix the one other caller of `ResolveExecutionSelector`**

Open `internal/gateway/batch_selection.go`. Line 99 currently reads:

```go
		resolvedSelector, _, err := ResolveExecutionSelector(ctx, provider, resolver, requested)
```

Change to (batch path doesn't need labels for this MVP — see spec §9/§10):

```go
		resolvedSelector, _, _, err := ResolveExecutionSelector(ctx, provider, resolver, requested)
```

- [ ] **Step 9: Run the full gateway package test suite**

Run: `go test ./internal/gateway/... -v`
Expected: PASS — both new tests plus every pre-existing test in the package (the 4-tuple threading must not change any existing resolution behavior).

- [ ] **Step 10: Run affected downstream packages**

Run: `go build ./... && go test ./internal/core/... ./internal/gateway/... ./internal/server/...`
Expected: PASS with no compile errors. (`internal/server/request_model_resolution.go` calls `gateway.ResolveRequestModel`/`ResolveRequestModelWithAuthorizer`, whose signatures are unchanged — only `ResolveExecutionSelector`'s signature changed, and its only two callers are inside `internal/gateway` itself, both fixed above.)

- [ ] **Step 11: Commit**

```bash
git add internal/core/request_model_resolution.go internal/gateway/interfaces.go internal/gateway/request_model_resolution.go internal/gateway/request_model_resolution_test.go internal/gateway/batch_selection.go
git commit -m "gateway: propagate optional resolver labels through RequestModelResolution

Adds LabelingModelResolver, an optional ModelResolver interface a resolver
can implement to report observability labels (e.g. task-classification
results) alongside the resolved selector, without changing the contract
other ModelResolver implementers rely on. Needed by the upcoming
internal/taskrouting P0 resolver."
```

---

### Task 2: Gateway — merge resolution labels into the request context

**Files:**
- Modify: `internal/gateway/inference_prepare.go`
- Test: `internal/gateway/inference_prepare_test.go` (new file)

**Why this task exists:** Task 1 gets labels onto `RequestModelResolution`, but nothing yet moves them into the `context.Context` that ends up on `PreparedChatRequest.Context` (which `LogUsage` reads labels from — `internal/gateway/usage.go:37`). `ensureTranslatedRequestWorkflow` calls `ResolveRequestModelWithAuthorizer` but currently discards everything except the `*core.Workflow` — its `ctx` parameter is never returned, so any local reassignment is invisible to its caller.

**Interfaces:**
- Consumes: `core.RequestModelResolution.Labels` (Task 1), `core.WithRequestLabels`/`core.MergeLabels`/`core.RequestLabelsFromContext` (existing, `internal/core/labels.go`).
- Produces: `PreparedChatRequest.Context`, `PreparedResponsesRequest.Context`, `PreparedEmbeddingRequest.Context` now carry merged labels when the active resolver reported any.

- [ ] **Step 1: Write failing tests in a new `inference_prepare_test.go`**

Create `internal/gateway/inference_prepare_test.go` (package `gateway`, reusing the `requestRefreshProvider`/`newRequestRefreshProvider`/`requestLabelingResolver` fakes from `request_model_resolution_test.go` — same package, no new imports needed for those):

```go
package gateway

import (
	"context"
	"testing"

	"smartrouter/internal/core"
)

func TestPrepareChatRequest_MergesResolverLabelsIntoContext(t *testing.T) {
	provider := newRequestRefreshProvider(1)
	provider.supported["deepseek/deepseek-coder"] = true
	provider.providerType["deepseek/deepseek-coder"] = "deepseek"
	resolver := requestLabelingResolver{
		selector: core.ModelSelector{Provider: "deepseek", Model: "deepseek-coder"},
		labels:   []string{"task:code"},
	}
	orchestrator := NewInferenceOrchestrator(InferenceConfig{
		Provider:      provider,
		ModelResolver: resolver,
	})

	prepared, err := orchestrator.PrepareChatRequest(context.Background(), &core.ChatRequest{Model: "smart-router/auto"}, RequestMeta{
		Endpoint: core.EndpointDescriptor{Operation: core.OperationChatCompletions},
	})
	if err != nil {
		t.Fatalf("PrepareChatRequest() error = %v, want nil", err)
	}
	labels := core.RequestLabelsFromContext(prepared.Context)
	if len(labels) != 1 || labels[0] != "task:code" {
		t.Fatalf("labels in prepared context = %v, want [task:code]", labels)
	}
}

func TestPrepareChatRequest_NoLabelsLeavesContextUnchanged(t *testing.T) {
	provider := newRequestRefreshProvider(1)
	orchestrator := NewInferenceOrchestrator(InferenceConfig{Provider: provider})

	prepared, err := orchestrator.PrepareChatRequest(context.Background(), &core.ChatRequest{Model: "openai/gpt-4o"}, RequestMeta{
		Endpoint: core.EndpointDescriptor{Operation: core.OperationChatCompletions},
	})
	if err != nil {
		t.Fatalf("PrepareChatRequest() error = %v, want nil", err)
	}
	if labels := core.RequestLabelsFromContext(prepared.Context); labels != nil {
		t.Fatalf("labels = %v, want nil", labels)
	}
}

func TestPrepareEmbeddingRequest_MergesResolverLabelsIntoContext(t *testing.T) {
	provider := newRequestRefreshProvider(1)
	provider.supported["deepseek/deepseek-embed"] = true
	provider.providerType["deepseek/deepseek-embed"] = "deepseek"
	resolver := requestLabelingResolver{
		selector: core.ModelSelector{Provider: "deepseek", Model: "deepseek-embed"},
		labels:   []string{"task:data_extraction"},
	}
	orchestrator := NewInferenceOrchestrator(InferenceConfig{
		Provider:      provider,
		ModelResolver: resolver,
	})

	prepared, err := orchestrator.PrepareEmbeddingRequest(context.Background(), &core.EmbeddingRequest{Model: "smart-router/auto"}, RequestMeta{
		Endpoint: core.EndpointDescriptor{Operation: core.OperationEmbeddings},
	})
	if err != nil {
		t.Fatalf("PrepareEmbeddingRequest() error = %v, want nil", err)
	}
	labels := core.RequestLabelsFromContext(prepared.Context)
	if len(labels) != 1 || labels[0] != "task:data_extraction" {
		t.Fatalf("labels in prepared context = %v, want [task:data_extraction]", labels)
	}
}
```

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `go test ./internal/gateway/... -run TestPrepareChatRequest_MergesResolverLabelsIntoContext -v`
Expected: FAIL — `TestPrepareChatRequest_MergesResolverLabelsIntoContext` should fail its assertion (labels come back nil) since `ensureTranslatedRequestWorkflow` doesn't merge labels into ctx yet. `TestPrepareEmbeddingRequest_...` fails the same way.

- [ ] **Step 3: Make `ensureTranslatedRequestWorkflow` return the updated context**

Open `internal/gateway/inference_prepare.go`. Replace `ensureTranslatedRequestWorkflow` (currently returning `(*core.Workflow, error)`):

```go
func (o *InferenceOrchestrator) ensureTranslatedRequestWorkflow(
	ctx context.Context,
	current *core.Workflow,
	requestID string,
	endpoint core.EndpointDescriptor,
	model,
	providerHint *string,
) (*core.Workflow, error) {
	if model == nil || providerHint == nil {
		return nil, core.NewInvalidRequestError("model selector targets are required", nil)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	workflow := currentTranslatedWorkflow(current, endpoint)
	var err error
	if workflow == nil {
		resolution, err := ResolveRequestModelWithAuthorizer(ctx, o.provider, o.modelResolver, o.modelAuthorizer, core.NewRequestedModelSelector(*model, *providerHint))
		if err != nil {
			return nil, err
		}
		workflow, err = TranslatedWorkflow(ctx, strings.TrimSpace(requestID), endpoint, resolution, o.workflowPolicyResolver)
		if err != nil {
			return nil, err
		}
		ApplyResolvedSelector(model, providerHint, resolution)
		return workflow, nil
	}

	resolution := workflow.Resolution
	if resolution != nil && o.modelAuthorizer != nil {
		if err := o.modelAuthorizer.ValidateModelAccess(ctx, resolution.ResolvedSelector); err != nil {
			return nil, err
		}
	}
	if resolution == nil {
		resolution, err = ResolveRequestModelWithAuthorizer(ctx, o.provider, o.modelResolver, o.modelAuthorizer, core.NewRequestedModelSelector(*model, *providerHint))
		if err != nil {
			return nil, err
		}
		workflow, err = TranslatedWorkflow(ctx, strings.TrimSpace(requestID), endpoint, resolution, o.workflowPolicyResolver)
		if err != nil {
			return nil, err
		}
	}
	ApplyResolvedSelector(model, providerHint, resolution)
	return workflow, nil
}
```

with:

```go
func (o *InferenceOrchestrator) ensureTranslatedRequestWorkflow(
	ctx context.Context,
	current *core.Workflow,
	requestID string,
	endpoint core.EndpointDescriptor,
	model,
	providerHint *string,
) (context.Context, *core.Workflow, error) {
	if model == nil || providerHint == nil {
		return ctx, nil, core.NewInvalidRequestError("model selector targets are required", nil)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	workflow := currentTranslatedWorkflow(current, endpoint)
	var err error
	if workflow == nil {
		resolution, err := ResolveRequestModelWithAuthorizer(ctx, o.provider, o.modelResolver, o.modelAuthorizer, core.NewRequestedModelSelector(*model, *providerHint))
		if err != nil {
			return ctx, nil, err
		}
		ctx = mergeResolutionLabels(ctx, resolution)
		workflow, err = TranslatedWorkflow(ctx, strings.TrimSpace(requestID), endpoint, resolution, o.workflowPolicyResolver)
		if err != nil {
			return ctx, nil, err
		}
		ApplyResolvedSelector(model, providerHint, resolution)
		return ctx, workflow, nil
	}

	resolution := workflow.Resolution
	if resolution != nil && o.modelAuthorizer != nil {
		if err := o.modelAuthorizer.ValidateModelAccess(ctx, resolution.ResolvedSelector); err != nil {
			return ctx, nil, err
		}
	}
	if resolution == nil {
		resolution, err = ResolveRequestModelWithAuthorizer(ctx, o.provider, o.modelResolver, o.modelAuthorizer, core.NewRequestedModelSelector(*model, *providerHint))
		if err != nil {
			return ctx, nil, err
		}
		ctx = mergeResolutionLabels(ctx, resolution)
		workflow, err = TranslatedWorkflow(ctx, strings.TrimSpace(requestID), endpoint, resolution, o.workflowPolicyResolver)
		if err != nil {
			return ctx, nil, err
		}
	}
	ApplyResolvedSelector(model, providerHint, resolution)
	return ctx, workflow, nil
}

// mergeResolutionLabels folds any observability labels a ModelResolver
// attached during resolution (e.g. TaskRouting's "task:code") into ctx, so
// they reach usage logging via the same context PreparedChatRequest carries
// forward. A resolution with no labels leaves ctx unchanged.
func mergeResolutionLabels(ctx context.Context, resolution *core.RequestModelResolution) context.Context {
	if resolution == nil || len(resolution.Labels) == 0 {
		return ctx
	}
	return core.WithRequestLabels(ctx, core.MergeLabels(core.RequestLabelsFromContext(ctx), resolution.Labels))
}
```

- [ ] **Step 4: Update the 3 call sites**

Same file, `prepareTranslatedRequest`. Change:

```go
	ctx = contextWithRequestID(ctx, meta.RequestID)
	workflow, err := o.ensureTranslatedRequestWorkflow(ctx, meta.Workflow, meta.RequestID, meta.Endpoint, model, provider)
```

to:

```go
	ctx = contextWithRequestID(ctx, meta.RequestID)
	ctx, workflow, err := o.ensureTranslatedRequestWorkflow(ctx, meta.Workflow, meta.RequestID, meta.Endpoint, model, provider)
```

Then `PrepareEmbeddingRequest`. Change:

```go
	ctx = contextWithRequestID(ctx, meta.RequestID)
	workflow, err := o.ensureTranslatedRequestWorkflow(ctx, meta.Workflow, meta.RequestID, meta.Endpoint, &req.Model, &req.Provider)
	if err != nil {
		return nil, err
	}
```

to:

```go
	ctx = contextWithRequestID(ctx, meta.RequestID)
	ctx, workflow, err := o.ensureTranslatedRequestWorkflow(ctx, meta.Workflow, meta.RequestID, meta.Endpoint, &req.Model, &req.Provider)
	if err != nil {
		return nil, err
	}
```

- [ ] **Step 5: Run the new tests to verify they pass**

Run: `go test ./internal/gateway/... -run 'TestPrepareChatRequest|TestPrepareEmbeddingRequest' -v`
Expected: PASS (all 3 new tests).

- [ ] **Step 6: Run the full gateway suite plus a full build**

Run: `go build ./... && go test ./internal/gateway/... -v`
Expected: PASS — this is the highest-risk mechanical change in the plan (a function used by `PrepareChatRequest`, `PrepareResponsesRequest`, and `PrepareEmbeddingRequest` changed its return arity), so a clean full-package pass here matters more than usual.

- [ ] **Step 7: Commit**

```bash
git add internal/gateway/inference_prepare.go internal/gateway/inference_prepare_test.go
git commit -m "gateway: merge resolver observability labels into the prepared request context

ensureTranslatedRequestWorkflow now returns its (possibly ctx.WithValue-updated)
context so labels a LabelingModelResolver attaches during resolution reach
PreparedChatRequest/PreparedResponsesRequest/PreparedEmbeddingRequest.Context,
and from there LogUsage, without any change to usage-layer code."
```

---

### Task 3: Config — `task_routing` configuration section

**Files:**
- Create: `config/taskrouting.go`
- Create: `config/taskrouting_test.go`
- Modify: `config/config.go`
- Modify: `config/config_test.go`
- Modify: `config/config.example.yaml`

**Interfaces:**
- Produces: `config.TaskRoutingConfig{Enabled bool, Entrypoints []string, Mappings []TaskRoutingMapping}`, `config.TaskRoutingMapping{Task, VirtualModelSource string}`, both consumed by `internal/taskrouting.New` (Task 6) and `internal/app/app.go` (Task 7) as `appCfg.TaskRouting`.
- Consumes: nothing from other tasks (config is self-contained, following the existing `config/tagging.go` pattern).

- [ ] **Step 1: Write failing tests in `config/taskrouting_test.go`**

Create `config/taskrouting_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestApplyTaskRoutingEnv_Overrides(t *testing.T) {
	cfg := &Config{TaskRouting: TaskRoutingConfig{Enabled: false}}
	t.Setenv("TASK_ROUTING_ENABLED", "true")
	if err := applyTaskRoutingEnv(cfg); err != nil {
		t.Fatalf("applyTaskRoutingEnv() error = %v", err)
	}
	if !cfg.TaskRouting.Enabled {
		t.Fatal("TASK_ROUTING_ENABLED=true did not override Enabled")
	}
}

func TestApplyTaskRoutingEnv_Unset(t *testing.T) {
	cfg := &Config{TaskRouting: TaskRoutingConfig{Enabled: true}}
	if err := applyTaskRoutingEnv(cfg); err != nil {
		t.Fatalf("applyTaskRoutingEnv() error = %v", err)
	}
	if !cfg.TaskRouting.Enabled {
		t.Fatal("no env var set should leave Enabled untouched")
	}
}

func TestNormalizeTaskRoutingConfig_DisabledSkipsValidation(t *testing.T) {
	cfg := &TaskRoutingConfig{Enabled: false}
	if err := normalizeTaskRoutingConfig(cfg); err != nil {
		t.Fatalf("normalizeTaskRoutingConfig() error = %v, want nil when disabled", err)
	}
}

func TestNormalizeTaskRoutingConfig_Valid(t *testing.T) {
	cfg := &TaskRoutingConfig{
		Enabled:     true,
		Entrypoints: []string{"smart-router/auto"},
		Mappings: []TaskRoutingMapping{
			{Task: "code", VirtualModelSource: "smart-router/tier-code"},
			{Task: "general", VirtualModelSource: "smart-router/tier-medium"},
		},
	}
	if err := normalizeTaskRoutingConfig(cfg); err != nil {
		t.Fatalf("normalizeTaskRoutingConfig() error = %v, want nil", err)
	}
}

func TestNormalizeTaskRoutingConfig_Rejections(t *testing.T) {
	tests := []struct {
		name string
		cfg  TaskRoutingConfig
	}{
		{
			name: "no entrypoints",
			cfg:  TaskRoutingConfig{Enabled: true, Mappings: []TaskRoutingMapping{{Task: "general", VirtualModelSource: "x"}}},
		},
		{
			name: "unknown task",
			cfg: TaskRoutingConfig{Enabled: true, Entrypoints: []string{"a"}, Mappings: []TaskRoutingMapping{
				{Task: "not_a_real_task", VirtualModelSource: "x"},
				{Task: "general", VirtualModelSource: "x"},
			}},
		},
		{
			name: "duplicate task",
			cfg: TaskRoutingConfig{Enabled: true, Entrypoints: []string{"a"}, Mappings: []TaskRoutingMapping{
				{Task: "code", VirtualModelSource: "x"},
				{Task: "code", VirtualModelSource: "y"},
				{Task: "general", VirtualModelSource: "x"},
			}},
		},
		{
			name: "empty virtual_model",
			cfg: TaskRoutingConfig{Enabled: true, Entrypoints: []string{"a"}, Mappings: []TaskRoutingMapping{
				{Task: "general", VirtualModelSource: ""},
			}},
		},
		{
			name: "missing general mapping",
			cfg: TaskRoutingConfig{Enabled: true, Entrypoints: []string{"a"}, Mappings: []TaskRoutingMapping{
				{Task: "code", VirtualModelSource: "x"},
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			if err := normalizeTaskRoutingConfig(&cfg); err == nil {
				t.Fatalf("expected error for %s", tt.name)
			}
		})
	}
}

func TestLoad_TaskRoutingFromYAMLAndEnv(t *testing.T) {
	clearAllConfigEnvVars(t)
	withTempDir(t, func(dir string) {
		yaml := `
task_routing:
  entrypoints:
    - smart-router/auto
  mappings:
    - { task: code, virtual_model: smart-router/tier-code }
    - { task: general, virtual_model: smart-router/tier-medium }
`
		if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0644); err != nil {
			t.Fatalf("failed to write config.yaml: %v", err)
		}
		t.Setenv("TASK_ROUTING_ENABLED", "true")

		result, err := Load()
		if err != nil {
			t.Fatalf("Load() error = %v", err)
		}
		tr := result.Config.TaskRouting
		if !tr.Enabled {
			t.Fatal("TASK_ROUTING_ENABLED=true did not enable task_routing")
		}
		if len(tr.Mappings) != 2 || tr.Mappings[0].Task != "code" {
			t.Fatalf("mappings = %#v, want 2 entries starting with code", tr.Mappings)
		}
	})
}
```

- [ ] **Step 2: Run the new tests to verify they fail to compile**

Run: `go test ./config/... -run TestNormalizeTaskRoutingConfig -v`
Expected: FAIL to compile — `TaskRoutingConfig`, `TaskRoutingMapping`, `applyTaskRoutingEnv`, `normalizeTaskRoutingConfig` don't exist yet.

- [ ] **Step 3: Create `config/taskrouting.go`**

```go
package config

import (
	"fmt"
	"os"
	"strings"
)

// TaskRoutingConfig declares the P0 rule-based task routing feature: which
// virtual model names trigger classification, and which virtual model each
// task type maps to. Disabled by default; when disabled, taskrouting.New
// returns the wrapped resolver unchanged and this config is not validated.
type TaskRoutingConfig struct {
	Enabled     bool                 `yaml:"enabled"`
	Entrypoints []string             `yaml:"entrypoints"`
	Mappings    []TaskRoutingMapping `yaml:"mappings"`
}

// TaskRoutingMapping declares one task type's target virtual model. Task must
// be one of validTaskRoutingTasks; VirtualModelSource is a "source" name
// declared under virtual_models and is not validated here since virtual
// models may also be declared dynamically via the admin store.
type TaskRoutingMapping struct {
	Task               string `yaml:"task"`
	VirtualModelSource string `yaml:"virtual_model"`
}

// validTaskRoutingTasks lists the task type strings the P0 rule classifier
// can emit (internal/taskrouting.TaskType). Duplicated here rather than
// imported so the config package does not depend on internal/taskrouting.
var validTaskRoutingTasks = map[string]bool{
	"agent_tool_use":   true,
	"code":             true,
	"translation":      true,
	"title_generation": true,
	"bulk_low_value":   true,
	"copywriting":      true,
	"data_extraction":  true,
	"general":          true,
}

// applyTaskRoutingEnv reads TASK_ROUTING_ENABLED, overriding the YAML value.
func applyTaskRoutingEnv(cfg *Config) error {
	if v, ok := os.LookupEnv("TASK_ROUTING_ENABLED"); ok {
		cfg.TaskRouting.Enabled = parseBool(v)
	}
	return nil
}

// normalizeTaskRoutingConfig validates task_routing when enabled. Disabled
// configs are not validated so an unused/half-filled section never blocks
// startup.
func normalizeTaskRoutingConfig(cfg *TaskRoutingConfig) error {
	if !cfg.Enabled {
		return nil
	}
	if len(cfg.Entrypoints) == 0 {
		return fmt.Errorf("task_routing.entrypoints must not be empty when task_routing.enabled is true")
	}
	seen := make(map[string]struct{}, len(cfg.Mappings))
	hasGeneral := false
	for i, m := range cfg.Mappings {
		task := strings.TrimSpace(m.Task)
		if !validTaskRoutingTasks[task] {
			return fmt.Errorf("task_routing.mappings[%d]: unknown task %q", i, m.Task)
		}
		if _, dup := seen[task]; dup {
			return fmt.Errorf("task_routing.mappings[%d]: duplicate task %q", i, m.Task)
		}
		seen[task] = struct{}{}
		if strings.TrimSpace(m.VirtualModelSource) == "" {
			return fmt.Errorf("task_routing.mappings[%d]: virtual_model must not be empty", i)
		}
		if task == "general" {
			hasGeneral = true
		}
	}
	if !hasGeneral {
		return fmt.Errorf(`task_routing.mappings must include a mapping for task "general" (used as the classifier fallback)`)
	}
	return nil
}
```

- [ ] **Step 4: Register the field on `config.Config` and wire it into `Load()`**

Open `config/config.go`. In the `Config` struct, add the field right after `Tagging`:

```go
	Tagging    TaggingConfig    `yaml:"tagging"`

	TaskRouting TaskRoutingConfig `yaml:"task_routing"`
```

In `Load()`, right after the existing tagging block:

```go
	if err := applyTaggingEnv(cfg); err != nil {
		return nil, err
	}
	if err := normalizeTaggingConfig(&cfg.Tagging); err != nil {
		return nil, err
	}
```

add:

```go
	if err := applyTaskRoutingEnv(cfg); err != nil {
		return nil, err
	}
	if err := normalizeTaskRoutingConfig(&cfg.TaskRouting); err != nil {
		return nil, err
	}
```

- [ ] **Step 5: Add `TASK_ROUTING_ENABLED` to `clearAllConfigEnvVars`**

Open `config/config_test.go`. In `clearAllConfigEnvVars`, add `"TASK_ROUTING_ENABLED"` to the string list (e.g. next to `"WORKFLOW_REFRESH_INTERVAL"`):

```go
		"WORKFLOW_REFRESH_INTERVAL",
		"TASK_ROUTING_ENABLED",
```

- [ ] **Step 6: Run the new tests to verify they pass**

Run: `go test ./config/... -run 'TaskRouting' -v`
Expected: PASS (all tests from Step 1).

- [ ] **Step 7: Run the full config package suite**

Run: `go test ./config/... -v`
Expected: PASS — confirms adding the field/env var didn't disturb any other `Load()`-level test (in particular tests that snapshot `buildDefaultConfig()` output or iterate all fields).

- [ ] **Step 8: Document the section in `config.example.yaml`**

Open `config/config.example.yaml`. After the commented `virtual_models:` example block (ends around line 60, right before the blank line preceding `cache:`), add:

```yaml

# TaskRouting (P0/MVP): rule-based task classification for virtual models
# marked as "smart" entrypoints. Disabled by default. When enabled, a request
# for one of `entrypoints` is classified (agent_tool_use, code, translation,
# title_generation, bulk_low_value, copywriting, data_extraction, or the
# general fallback) using request content and existing tagging labels, then
# rewritten to the mapped virtual_model name before the normal virtual_models
# resolution/load-balancing/failover pipeline runs unchanged. A "general"
# mapping is required as the classifier fallback. See
# docs/superpowers/specs/2026-07-27-taskrouting-design.md for the full design.
# task_routing:
#   enabled: false # env: TASK_ROUTING_ENABLED
#   entrypoints:
#     - smart-router/auto
#   mappings:
#     - { task: title_generation, virtual_model: smart-router/tier-simple }
#     - { task: bulk_low_value,    virtual_model: smart-router/tier-simple }
#     - { task: copywriting,       virtual_model: smart-router/tier-medium }
#     - { task: translation,       virtual_model: smart-router/tier-medium }
#     - { task: data_extraction,   virtual_model: smart-router/tier-medium }
#     - { task: code,              virtual_model: smart-router/tier-code }
#     - { task: agent_tool_use,    virtual_model: smart-router/tier-quality }
#     - { task: general,           virtual_model: smart-router/tier-medium }
```

- [ ] **Step 9: Commit**

```bash
git add config/taskrouting.go config/taskrouting_test.go config/config.go config/config_test.go config/config.example.yaml
git commit -m "config: add task_routing configuration section

Adds the P0 TaskRouting config surface (enabled flag, entrypoints,
task-to-virtual-model mappings) with TASK_ROUTING_ENABLED env override and
startup-time validation, following the existing tagging config pattern."
```

---

### Task 4: taskrouting — core types and the P0 rule classifier

**Files:**
- Create: `internal/taskrouting/types.go`
- Create: `internal/taskrouting/classifier.go`
- Create: `internal/taskrouting/rules.go`
- Test: `internal/taskrouting/rules_test.go`

**Interfaces:**
- Produces: `taskrouting.TaskType` (and its 8 constants), `taskrouting.Classification{Task, Reason}`, `taskrouting.Message{Role, Content}`, `taskrouting.ClassificationInput{Messages, HasToolCalls, ToolNames, RequestLabels, RequestedModel}`, `taskrouting.Classifier` interface, `taskrouting.NewRuleClassifier() *RuleClassifier`.
- Consumes: nothing from other tasks (pure package-internal logic; no dependency on `gateway`/`config` yet — those come in Tasks 5–6).

- [ ] **Step 1: Create `internal/taskrouting/types.go`**

```go
// Package taskrouting classifies chat/responses requests into a coarse task
// type and rewrites "smart-router/auto"-style virtual model requests to a
// task-specific virtual model, reusing internal/virtualmodels for the actual
// provider/target selection, load balancing, and failover.
package taskrouting

// TaskType is a coarse task category used to pick a cost/quality tier.
type TaskType string

const (
	TaskAgentToolUse    TaskType = "agent_tool_use"
	TaskCode            TaskType = "code"
	TaskTranslation     TaskType = "translation"
	TaskTitleGeneration TaskType = "title_generation"
	TaskBulkLowValue    TaskType = "bulk_low_value"
	TaskCopywriting     TaskType = "copywriting"
	TaskDataExtraction  TaskType = "data_extraction"
	TaskGeneral         TaskType = "general"
)

// Classification is a classifier's decision for one request.
type Classification struct {
	Task   TaskType
	Reason string // matched rule name, for logging/debugging
}

// Message is the minimal chat message shape a classifier needs.
type Message struct {
	Role    string
	Content string
}

// ClassificationInput carries only the signals a classifier needs, kept
// independent of core.ChatRequest so classifiers don't depend on transport types.
type ClassificationInput struct {
	Messages       []Message
	HasToolCalls   bool
	ToolNames      []string
	RequestLabels  []string // existing tagging labels (e.g. "batch") on this request
	RequestedModel string   // the entrypoint alias requested, for logging only
}

// LastUserMessage returns the content of the last message with Role == "user",
// or "" if there is none.
func (in ClassificationInput) LastUserMessage() string {
	for i := len(in.Messages) - 1; i >= 0; i-- {
		if in.Messages[i].Role == "user" {
			return in.Messages[i].Content
		}
	}
	return ""
}
```

- [ ] **Step 2: Create `internal/taskrouting/classifier.go`**

```go
package taskrouting

import "context"

// Classifier decides a TaskType for one chat/responses request. The P0 MVP
// ships one implementation, RuleClassifier; the interface exists so a future
// LLM-based classifier (or a rule+LLM chain) can be swapped in later without
// changing Resolver.
type Classifier interface {
	Classify(ctx context.Context, input ClassificationInput) (Classification, error)
}
```

- [ ] **Step 3: Write failing tests in `rules_test.go`**

Create `internal/taskrouting/rules_test.go`:

```go
package taskrouting

import (
	"context"
	"testing"
)

func classify(t *testing.T, in ClassificationInput) Classification {
	t.Helper()
	c := NewRuleClassifier()
	got, err := c.Classify(context.Background(), in)
	if err != nil {
		t.Fatalf("Classify() error = %v, want nil", err)
	}
	return got
}

func TestRuleClassifier_AgentToolUse(t *testing.T) {
	got := classify(t, ClassificationInput{HasToolCalls: true})
	if got.Task != TaskAgentToolUse {
		t.Fatalf("Task = %q, want %q", got.Task, TaskAgentToolUse)
	}
}

func TestRuleClassifier_CodeFence(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "fix this: ```go\nfunc main() {}\n```"},
	}})
	if got.Task != TaskCode {
		t.Fatalf("Task = %q, want %q", got.Task, TaskCode)
	}
}

func TestRuleClassifier_CodeKeywords(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "please write a func that uses import os and returns"},
	}})
	if got.Task != TaskCode {
		t.Fatalf("Task = %q, want %q", got.Task, TaskCode)
	}
}

func TestRuleClassifier_Translation(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "把这段话翻译成英文：你好"},
	}})
	if got.Task != TaskTranslation {
		t.Fatalf("Task = %q, want %q", got.Task, TaskTranslation)
	}
}

func TestRuleClassifier_TitleGeneration(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "给这篇文章起个标题"},
	}})
	if got.Task != TaskTitleGeneration {
		t.Fatalf("Task = %q, want %q", got.Task, TaskTitleGeneration)
	}
}

func TestRuleClassifier_BulkLowValue(t *testing.T) {
	got := classify(t, ClassificationInput{
		Messages:      []Message{{Role: "user", Content: "summarize this"}},
		RequestLabels: []string{"batch"},
	})
	if got.Task != TaskBulkLowValue {
		t.Fatalf("Task = %q, want %q", got.Task, TaskBulkLowValue)
	}
}

func TestRuleClassifier_Copywriting(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "帮我写一篇产品文案"},
	}})
	if got.Task != TaskCopywriting {
		t.Fatalf("Task = %q, want %q", got.Task, TaskCopywriting)
	}
}

func TestRuleClassifier_DataExtraction(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "从这段文本中提取姓名和日期，按 JSON schema 输出"},
	}})
	if got.Task != TaskDataExtraction {
		t.Fatalf("Task = %q, want %q", got.Task, TaskDataExtraction)
	}
}

func TestRuleClassifier_FallsBackToGeneral(t *testing.T) {
	got := classify(t, ClassificationInput{Messages: []Message{
		{Role: "user", Content: "What's the weather like today?"},
	}})
	if got.Task != TaskGeneral {
		t.Fatalf("Task = %q, want %q", got.Task, TaskGeneral)
	}
	if got.Reason != "no_rule_matched" {
		t.Fatalf("Reason = %q, want %q", got.Reason, "no_rule_matched")
	}
}
```

- [ ] **Step 4: Run the new tests to verify they fail**

Run: `go test ./internal/taskrouting/... -v`
Expected: FAIL to compile — `NewRuleClassifier` doesn't exist yet.

- [ ] **Step 5: Create `internal/taskrouting/rules.go`**

```go
package taskrouting

import (
	"context"
	"strings"
)

// rule is one ordered step in RuleClassifier's chain. Match reports whether
// this rule applies to input; the first matching rule wins.
type rule struct {
	name  string
	task  TaskType
	match func(ClassificationInput) bool
}

// RuleClassifier is the P0 MVP Classifier: a fixed, ordered chain of
// keyword/structure heuristics, falling back to TaskGeneral when nothing
// matches. Pure string/regex matching — no external calls, microsecond-level
// cost per request.
type RuleClassifier struct {
	rules []rule
}

// NewRuleClassifier builds the default P0 rule chain (see
// docs/superpowers/specs/2026-07-27-taskrouting-design.md §3.2).
func NewRuleClassifier() *RuleClassifier {
	return &RuleClassifier{rules: defaultRules}
}

func (c *RuleClassifier) Classify(_ context.Context, input ClassificationInput) (Classification, error) {
	for _, r := range c.rules {
		if r.match(input) {
			return Classification{Task: r.task, Reason: r.name}, nil
		}
	}
	return Classification{Task: TaskGeneral, Reason: "no_rule_matched"}, nil
}

var defaultRules = []rule{
	{name: "agent_tool_use", task: TaskAgentToolUse, match: matchAgentToolUse},
	{name: "code", task: TaskCode, match: matchCode},
	{name: "translation", task: TaskTranslation, match: matchTranslation},
	{name: "title_generation", task: TaskTitleGeneration, match: matchTitleGeneration},
	{name: "bulk_low_value", task: TaskBulkLowValue, match: matchBulkLowValue},
	{name: "copywriting", task: TaskCopywriting, match: matchCopywriting},
	{name: "data_extraction", task: TaskDataExtraction, match: matchDataExtraction},
}

func matchAgentToolUse(in ClassificationInput) bool {
	return in.HasToolCalls
}

var codeKeywords = []string{"func ", "def ", "class ", "import ", "package "}

func matchCode(in ClassificationInput) bool {
	msg := in.LastUserMessage()
	if strings.Contains(msg, "```") {
		return true
	}
	lower := strings.ToLower(msg)
	hits := 0
	for _, kw := range codeKeywords {
		if strings.Contains(lower, kw) {
			hits++
		}
	}
	return hits >= 2
}

var translationPhrases = []string{"翻译成", "翻译为", "译为", "translate to", "translate into"}

func matchTranslation(in ClassificationInput) bool {
	lower := strings.ToLower(in.LastUserMessage())
	for _, phrase := range translationPhrases {
		if strings.Contains(lower, strings.ToLower(phrase)) {
			return true
		}
	}
	return false
}

func matchTitleGeneration(in ClassificationInput) bool {
	msg := in.LastUserMessage()
	if len([]rune(msg)) >= 50 {
		return false
	}
	return strings.Contains(msg, "标题") || strings.Contains(strings.ToLower(msg), "title")
}

func matchBulkLowValue(in ClassificationInput) bool {
	for _, label := range in.RequestLabels {
		if strings.EqualFold(strings.TrimSpace(label), "batch") {
			return true
		}
	}
	return false
}

var copywritingKeywords = []string{"写一篇", "文案", "slogan", "营销"}

func matchCopywriting(in ClassificationInput) bool {
	msg := in.LastUserMessage()
	lower := strings.ToLower(msg)
	for _, kw := range copywritingKeywords {
		if strings.Contains(msg, kw) || strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

var dataExtractionKeywords = []string{"提取", "抽取", "extract", "json schema", "结构化输出"}

func matchDataExtraction(in ClassificationInput) bool {
	msg := in.LastUserMessage()
	lower := strings.ToLower(msg)
	for _, kw := range dataExtractionKeywords {
		if strings.Contains(msg, kw) || strings.Contains(lower, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/taskrouting/... -v`
Expected: PASS (all 9 tests).

- [ ] **Step 7: Commit**

```bash
git add internal/taskrouting/types.go internal/taskrouting/classifier.go internal/taskrouting/rules.go internal/taskrouting/rules_test.go
git commit -m "taskrouting: add P0 rule-based classifier

Implements the ordered rule chain from the TaskRouting design spec §3.2
(agent_tool_use, code, translation, title_generation, bulk_low_value,
copywriting, data_extraction, falling back to general). Pure string
matching, no external dependencies."
```

---

### Task 5: taskrouting — Resolver

**Files:**
- Create: `internal/taskrouting/resolver.go`
- Test: `internal/taskrouting/resolver_test.go`

**Why `buildClassificationInput` reads `core.RequestSnapshot` instead of the request struct:** `gateway.ModelResolver.ResolveModel(requested core.RequestedModelSelector)` only ever receives a model-name string — by the time model resolution runs, nothing in the call chain (`ensureTranslatedRequestWorkflow` → `ResolveRequestModelWithAuthorizer` → `ResolveExecutionSelector` → the resolver) has access to the actual `core.ChatRequest.Messages`/`Tools`. The design spec's pseudocode glossed over this as `buildInput(ctx, requested)`. Investigation found the actual mechanism already in the codebase: `internal/server/request_snapshot.go`'s `RequestSnapshotCapture` middleware (which runs early in the HTTP middleware chain, well before workflow resolution — see `CLAUDE.md`'s documented order) attaches a `*core.RequestSnapshot` to the request context via `core.WithRequestSnapshot`, carrying the raw captured JSON body (`snapshot.CapturedBodyView()`). That same `ctx` is what flows unbroken into `Resolver.ResolveModel(ForUserPath)`. So the classifier parses the raw JSON body straight off the snapshot. This degrades gracefully: if the snapshot is absent, or the body exceeded the 64KB inline capture limit (`BodyNotCaptured == true`), or JSON parsing fails, `buildClassificationInput` returns a signal-free `ClassificationInput{}`, which the rule chain resolves to `TaskGeneral` — never an error, never a blocked request.

**Interfaces:**
- Consumes: `gateway.ModelResolver`, `gateway.UserPathModelResolver`, `gateway.LabelingModelResolver` (Task 1), `taskrouting.Classifier`/`ClassificationInput`/`Message`/`TaskType` (Task 4), `core.GetRequestSnapshot`, `core.RequestLabelsFromContext` (existing).
- Produces: `taskrouting.NewResolver(next gateway.ModelResolver, classifier Classifier, mappings map[TaskType]string, entrypoints []string) *Resolver`, implementing all three gateway interfaces above. Consumed by `taskrouting.New` (Task 6).

- [ ] **Step 1: Write failing tests in `resolver_test.go`**

Create `internal/taskrouting/resolver_test.go`:

```go
package taskrouting

import (
	"context"
	"errors"
	"testing"

	"smartrouter/internal/core"
	"smartrouter/internal/gateway"
)

type fakeNext struct {
	calls         int
	userPathCalls int
	lastRequested core.RequestedModelSelector
	resolveErr    error
}

func (f *fakeNext) ResolveModel(requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	f.calls++
	f.lastRequested = requested
	if f.resolveErr != nil {
		return core.ModelSelector{}, false, f.resolveErr
	}
	selector, err := requested.Normalize()
	return selector, false, err
}

func (f *fakeNext) ResolveModelForUserPath(_ context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	f.userPathCalls++
	return f.ResolveModel(requested)
}

type fakeClassifier struct {
	result Classification
	err    error
	calls  int
}

func (f *fakeClassifier) Classify(_ context.Context, _ ClassificationInput) (Classification, error) {
	f.calls++
	return f.result, f.err
}

func TestResolver_NonEntrypointPassesThroughWithoutClassifying(t *testing.T) {
	next := &fakeNext{}
	classifier := &fakeClassifier{}
	resolver := NewResolver(next, classifier, map[TaskType]string{TaskGeneral: "smart-router/tier-medium"}, []string{"smart-router/auto"})

	_, _, err := resolver.ResolveModel(core.NewRequestedModelSelector("openai/gpt-4o", ""))
	if err != nil {
		t.Fatalf("ResolveModel() error = %v, want nil", err)
	}
	if classifier.calls != 0 {
		t.Fatalf("classifier.calls = %d, want 0 for a non-entrypoint model", classifier.calls)
	}
	if next.calls != 1 {
		t.Fatalf("next.calls = %d, want 1", next.calls)
	}
	if next.lastRequested.Model != "openai/gpt-4o" {
		t.Fatalf("next received %q, want unchanged %q", next.lastRequested.Model, "openai/gpt-4o")
	}
}

func TestResolver_EntrypointRewritesToMappedVirtualModelAndReturnsLabel(t *testing.T) {
	next := &fakeNext{}
	classifier := &fakeClassifier{result: Classification{Task: TaskCode, Reason: "code"}}
	resolver := NewResolver(next, classifier, map[TaskType]string{TaskCode: "smart-router/tier-code"}, []string{"smart-router/auto"})

	selector, _, labels, err := resolver.ResolveModelWithLabels(context.Background(), core.NewRequestedModelSelector("smart-router/auto", ""))
	if err != nil {
		t.Fatalf("ResolveModelWithLabels() error = %v, want nil", err)
	}
	if classifier.calls != 1 {
		t.Fatalf("classifier.calls = %d, want 1", classifier.calls)
	}
	if next.lastRequested.Model != "smart-router/tier-code" {
		t.Fatalf("next received %q, want rewritten %q", next.lastRequested.Model, "smart-router/tier-code")
	}
	if selector.QualifiedModel() != "smart-router/tier-code" {
		t.Fatalf("selector.QualifiedModel() = %q, want %q", selector.QualifiedModel(), "smart-router/tier-code")
	}
	if len(labels) != 1 || labels[0] != "task:code" {
		t.Fatalf("labels = %v, want [task:code]", labels)
	}
}

func TestResolver_ClassifierErrorFailsOpenToOriginalModel(t *testing.T) {
	next := &fakeNext{}
	classifier := &fakeClassifier{err: errors.New("boom")}
	resolver := NewResolver(next, classifier, map[TaskType]string{TaskGeneral: "smart-router/tier-medium"}, []string{"smart-router/auto"})

	_, _, labels, err := resolver.ResolveModelWithLabels(context.Background(), core.NewRequestedModelSelector("smart-router/auto", ""))
	if err != nil {
		t.Fatalf("ResolveModelWithLabels() error = %v, want nil (fail-open)", err)
	}
	if labels != nil {
		t.Fatalf("labels = %v, want nil on classifier error", labels)
	}
	if next.lastRequested.Model != "smart-router/auto" {
		t.Fatalf("next received %q, want original %q (fail-open)", next.lastRequested.Model, "smart-router/auto")
	}
}

func TestResolver_UnmappedTaskFailsOpenToOriginalModel(t *testing.T) {
	next := &fakeNext{}
	classifier := &fakeClassifier{result: Classification{Task: TaskCode, Reason: "code"}}
	resolver := NewResolver(next, classifier, map[TaskType]string{TaskGeneral: "smart-router/tier-medium"}, []string{"smart-router/auto"})

	_, _, labels, err := resolver.ResolveModelWithLabels(context.Background(), core.NewRequestedModelSelector("smart-router/auto", ""))
	if err != nil {
		t.Fatalf("ResolveModelWithLabels() error = %v, want nil (fail-open)", err)
	}
	if labels != nil {
		t.Fatalf("labels = %v, want nil when task has no mapping", labels)
	}
	if next.lastRequested.Model != "smart-router/auto" {
		t.Fatalf("next received %q, want original %q (fail-open)", next.lastRequested.Model, "smart-router/auto")
	}
}

func TestResolver_PrefersUserPathDelegateWhenNextImplementsIt(t *testing.T) {
	next := &fakeNext{}
	classifier := &fakeClassifier{result: Classification{Task: TaskGeneral}}
	resolver := NewResolver(next, classifier, map[TaskType]string{TaskGeneral: "smart-router/tier-medium"}, []string{"smart-router/auto"})

	if _, _, err := resolver.ResolveModelForUserPath(context.Background(), core.NewRequestedModelSelector("smart-router/auto", "")); err != nil {
		t.Fatalf("ResolveModelForUserPath() error = %v, want nil", err)
	}
	if next.userPathCalls != 1 {
		t.Fatalf("next.userPathCalls = %d, want 1 (delegate should prefer ResolveModelForUserPath)", next.userPathCalls)
	}
}

var (
	_ gateway.ModelResolver         = (*Resolver)(nil)
	_ gateway.UserPathModelResolver = (*Resolver)(nil)
	_ gateway.LabelingModelResolver = (*Resolver)(nil)
)

// buildClassificationInput is tested directly and independently below rather
// than only indirectly through Resolver, since it's the piece responsible for
// degrading gracefully when no request snapshot is available.

func TestBuildClassificationInput_ParsesMessagesAndToolsFromSnapshot(t *testing.T) {
	body := []byte(`{"model":"smart-router/auto","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function"}]}`)
	snapshot := core.NewRequestSnapshotWithOwnedMaps("POST", "/v1/chat/completions", nil, nil, nil, "application/json", body, false, "req-1", nil)
	ctx := core.WithRequestSnapshot(context.Background(), snapshot)

	got := buildClassificationInput(ctx, core.NewRequestedModelSelector("smart-router/auto", ""))
	if len(got.Messages) != 1 || got.Messages[0].Content != "hello" || got.Messages[0].Role != "user" {
		t.Fatalf("Messages = %v, want [{user hello}]", got.Messages)
	}
	if !got.HasToolCalls {
		t.Fatal("HasToolCalls = false, want true (tools array present)")
	}
}

func TestBuildClassificationInput_NoSnapshotIsSignalFree(t *testing.T) {
	got := buildClassificationInput(context.Background(), core.NewRequestedModelSelector("smart-router/auto", ""))
	if len(got.Messages) != 0 || got.HasToolCalls {
		t.Fatalf("got = %+v, want signal-free input", got)
	}
}

func TestBuildClassificationInput_BodyNotCapturedIsSignalFree(t *testing.T) {
	snapshot := core.NewRequestSnapshotWithOwnedMaps("POST", "/v1/chat/completions", nil, nil, nil, "application/json", nil, true, "req-1", nil)
	ctx := core.WithRequestSnapshot(context.Background(), snapshot)

	got := buildClassificationInput(ctx, core.NewRequestedModelSelector("smart-router/auto", ""))
	if len(got.Messages) != 0 || got.HasToolCalls {
		t.Fatalf("got = %+v, want signal-free input when body wasn't captured", got)
	}
}
```

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `go test ./internal/taskrouting/... -v`
Expected: FAIL to compile — `NewResolver`, `Resolver`, `buildClassificationInput` don't exist yet.

- [ ] **Step 3: Create `internal/taskrouting/resolver.go`**

```go
package taskrouting

import (
	"context"
	"strings"

	"github.com/goccy/go-json"

	"smartrouter/internal/core"
	"smartrouter/internal/gateway"
)

// Resolver wraps another gateway.ModelResolver (normally *virtualmodels.Service),
// intercepting only requests whose model is a configured entrypoint. All other
// requests are delegated untouched at zero extra cost — no ClassificationInput
// is built and no rules run. Any classifier error or unmapped task type fails
// open: the original requested model is delegated as if TaskRouting were not
// installed, so TaskRouting can never be the reason a request fails.
type Resolver struct {
	next        gateway.ModelResolver
	classifier  Classifier
	mappings    map[TaskType]string
	entrypoints map[string]struct{}
}

// NewResolver builds a Resolver. mappings and entrypoints are expected to be
// pre-validated (see config.normalizeTaskRoutingConfig); NewResolver does not
// re-validate them.
func NewResolver(next gateway.ModelResolver, classifier Classifier, mappings map[TaskType]string, entrypoints []string) *Resolver {
	set := make(map[string]struct{}, len(entrypoints))
	for _, e := range entrypoints {
		set[e] = struct{}{}
	}
	return &Resolver{next: next, classifier: classifier, mappings: mappings, entrypoints: set}
}

// ResolveModel satisfies gateway.ModelResolver.
func (r *Resolver) ResolveModel(requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	selector, changed, _, err := r.resolveModel(context.Background(), requested)
	return selector, changed, err
}

// ResolveModelForUserPath satisfies gateway.UserPathModelResolver.
func (r *Resolver) ResolveModelForUserPath(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	selector, changed, _, err := r.resolveModel(ctx, requested)
	return selector, changed, err
}

// ResolveModelWithLabels satisfies gateway.LabelingModelResolver, returning the
// classification result (e.g. "task:code") as an observability label.
func (r *Resolver) ResolveModelWithLabels(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error) {
	return r.resolveModel(ctx, requested)
}

func (r *Resolver) resolveModel(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, []string, error) {
	if _, ok := r.entrypoints[requested.Model]; !ok {
		selector, changed, err := r.delegate(ctx, requested)
		return selector, changed, nil, err
	}
	classification, err := r.classifier.Classify(ctx, buildClassificationInput(ctx, requested))
	if err != nil {
		selector, changed, delErr := r.delegate(ctx, requested)
		return selector, changed, nil, delErr
	}
	target, ok := r.mappings[classification.Task]
	if !ok {
		selector, changed, delErr := r.delegate(ctx, requested)
		return selector, changed, nil, delErr
	}
	rewritten := core.NewRequestedModelSelector(target, requested.ProviderHint)
	selector, changed, delErr := r.delegate(ctx, rewritten)
	if delErr != nil {
		return selector, changed, nil, delErr
	}
	return selector, changed, []string{"task:" + string(classification.Task)}, nil
}

func (r *Resolver) delegate(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	if scoped, ok := r.next.(gateway.UserPathModelResolver); ok {
		return scoped.ResolveModelForUserPath(ctx, requested)
	}
	return r.next.ResolveModel(requested)
}

// buildClassificationInput extracts the signals RuleClassifier needs from the
// raw request body captured at HTTP ingress (core.RequestSnapshot), since the
// gateway.ModelResolver mounting point only receives the requested model
// string, not the parsed core.ChatRequest. Falls back to a signal-free input
// (which the rule chain resolves to TaskGeneral) when no snapshot is present,
// the body was too large to capture, or it fails to parse — classification
// never blocks the request.
func buildClassificationInput(ctx context.Context, requested core.RequestedModelSelector) ClassificationInput {
	input := ClassificationInput{
		RequestLabels:  core.RequestLabelsFromContext(ctx),
		RequestedModel: requested.Model,
	}
	snapshot := core.GetRequestSnapshot(ctx)
	if snapshot == nil || snapshot.BodyNotCaptured {
		return input
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
	}
	if err := json.Unmarshal(snapshot.CapturedBodyView(), &body); err != nil {
		return input
	}
	input.HasToolCalls = len(body.Tools) > 0 || hasForcedToolChoice(body.ToolChoice)
	input.Messages = make([]Message, 0, len(body.Messages))
	for _, m := range body.Messages {
		input.Messages = append(input.Messages, Message{Role: m.Role, Content: plainTextContent(m.Content)})
	}
	return input
}

// plainTextContent decodes raw as a JSON string (the common chat message
// shape). Multimodal array-shaped content is not decoded for classification
// purposes in the P0 rule classifier — it yields "", meaning rules that key
// on message text simply won't match that message.
func plainTextContent(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// hasForcedToolChoice reports whether tool_choice forces tool use: a
// non-empty, non-"none" string, or any object/array shape (a forced function
// selector).
func hasForcedToolChoice(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s != "" && !strings.EqualFold(s, "none")
	}
	return true
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/taskrouting/... -v`
Expected: PASS (all resolver + buildClassificationInput tests, plus Task 4's rule tests still passing).

- [ ] **Step 5: Run `go vet` on the new package**

Run: `go vet ./internal/taskrouting/...`
Expected: clean (no warnings).

- [ ] **Step 6: Commit**

```bash
git add internal/taskrouting/resolver.go internal/taskrouting/resolver_test.go
git commit -m "taskrouting: add Resolver wrapping gateway.ModelResolver

Resolver intercepts only configured entrypoint model names, classifies via
the rule chain using signals read from core.RequestSnapshot (the only place
message/tool-call content is available at this mounting point), rewrites to
the mapped virtual model, and delegates everything else — including all
failure paths — to the wrapped resolver unchanged (fail-open)."
```

---

### Task 6: taskrouting — factory

**Files:**
- Create: `internal/taskrouting/factory.go`
- Test: `internal/taskrouting/factory_test.go`

**Interfaces:**
- Consumes: `config.TaskRoutingConfig`/`TaskRoutingMapping` (Task 3), `taskrouting.NewResolver`/`NewRuleClassifier` (Tasks 4–5).
- Produces: `taskrouting.New(cfg config.TaskRoutingConfig, next gateway.ModelResolver) (gateway.ModelResolver, error)`, consumed by `internal/app/app.go` (Task 7).

- [ ] **Step 1: Write failing tests in `factory_test.go`**

Create `internal/taskrouting/factory_test.go`:

```go
package taskrouting

import (
	"testing"

	"smartrouter/config"
	"smartrouter/internal/core"
)

type fakeNextResolver struct {
	calls int
}

func (f *fakeNextResolver) ResolveModel(requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	f.calls++
	selector, err := requested.Normalize()
	return selector, false, err
}

func TestNew_DisabledReturnsNextUnchanged(t *testing.T) {
	next := &fakeNextResolver{}
	resolver, err := New(config.TaskRoutingConfig{Enabled: false}, next)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if _, ok := resolver.(*Resolver); ok {
		t.Fatal("New() wrapped next even though task_routing.enabled is false")
	}
	if _, _, err := resolver.ResolveModel(core.NewRequestedModelSelector("openai/gpt-4o", "")); err != nil {
		t.Fatalf("ResolveModel() error = %v, want nil", err)
	}
	if next.calls != 1 {
		t.Fatalf("next.calls = %d, want 1 (disabled New() must return next directly)", next.calls)
	}
}

func TestNew_EnabledWrapsWithResolver(t *testing.T) {
	next := &fakeNextResolver{}
	cfg := config.TaskRoutingConfig{
		Enabled:     true,
		Entrypoints: []string{"smart-router/auto"},
		Mappings: []config.TaskRoutingMapping{
			{Task: "code", VirtualModelSource: "smart-router/tier-code"},
			{Task: "general", VirtualModelSource: "smart-router/tier-medium"},
		},
	}
	resolver, err := New(cfg, next)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if _, ok := resolver.(*Resolver); !ok {
		t.Fatal("New() did not wrap next in *Resolver when enabled")
	}
}

func TestNew_EnabledWithoutNextErrors(t *testing.T) {
	cfg := config.TaskRoutingConfig{Enabled: true, Entrypoints: []string{"smart-router/auto"}}
	if _, err := New(cfg, nil); err == nil {
		t.Fatal("New() error = nil, want error for nil next when enabled")
	}
}
```

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `go test ./internal/taskrouting/... -run TestNew_ -v`
Expected: FAIL to compile — `New` doesn't exist yet.

- [ ] **Step 3: Create `internal/taskrouting/factory.go`**

```go
package taskrouting

import (
	"fmt"

	"smartrouter/config"
	"smartrouter/internal/gateway"
)

// New builds the P0 TaskRouting resolver from cfg, wrapping next. When
// cfg.Enabled is false, New returns next unchanged so callers (internal/app)
// can always wrap unconditionally without a behavior change when the feature
// is off. cfg is expected to already be validated by
// config.normalizeTaskRoutingConfig (called from config.Load()).
func New(cfg config.TaskRoutingConfig, next gateway.ModelResolver) (gateway.ModelResolver, error) {
	if !cfg.Enabled {
		return next, nil
	}
	if next == nil {
		return nil, fmt.Errorf("taskrouting: next resolver is required when task_routing.enabled is true")
	}
	mappings := make(map[TaskType]string, len(cfg.Mappings))
	for _, m := range cfg.Mappings {
		mappings[TaskType(m.Task)] = m.VirtualModelSource
	}
	return NewResolver(next, NewRuleClassifier(), mappings, cfg.Entrypoints), nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/taskrouting/... -v`
Expected: PASS — every test in the package (Tasks 4–6 combined).

- [ ] **Step 5: Commit**

```bash
git add internal/taskrouting/factory.go internal/taskrouting/factory_test.go
git commit -m "taskrouting: add New() factory bridging config.TaskRoutingConfig to Resolver

New returns the wrapped resolver unchanged when task_routing.enabled is
false, so internal/app can wrap unconditionally with zero behavior change
by default."
```

---

### Task 7: Wire TaskRouting into app assembly

**Files:**
- Modify: `internal/app/app.go`

**Interfaces:**
- Consumes: `taskrouting.New` (Task 6), `appCfg.TaskRouting` (Task 3).
- Produces: nothing new — this task only rewires two existing `ModelResolver:` assignments.

- [ ] **Step 1: Add the import**

Open `internal/app/app.go`. In the import block, insert `"smartrouter/internal/taskrouting"` alphabetically between `"smartrouter/internal/tagging"` and `"smartrouter/internal/usage"`:

```go
	"smartrouter/internal/tagging"
	"smartrouter/internal/taskrouting"
	"smartrouter/internal/usage"
```

- [ ] **Step 2: Wrap `vm` right after it's defined**

Around line 275, `vm := app.virtualModels.Service` is followed immediately by the `failoverResult` block. Insert between them:

```go
	vm := app.virtualModels.Service

	modelResolver, err := taskrouting.New(appCfg.TaskRouting, vm)
	if err != nil {
		return fail("failed to initialize task routing", err)
	}

	var failoverResult *failover.Result
```

- [ ] **Step 3: Use `modelResolver` at the two `ModelResolver:` sites**

Around line 434, inside the `serverCfg := &server.Config{...}` literal, change:

```go
		ModelResolver:                   vm,
```

to:

```go
		ModelResolver:                   modelResolver,
```

(Leave `ModelAuthorizer: vm` and `ExposedModelLister: vm` on the same struct unchanged — `taskrouting.Resolver` only implements the `ModelResolver` family, not `ModelAuthorizer` or the exposed-model listing interface.)

Around line 540, inside the `server.NewInternalChatCompletionExecutor(provider, server.InternalChatCompletionExecutorConfig{...})` call, change:

```go
		ModelResolver:          vm,
```

to:

```go
		ModelResolver:          modelResolver,
```

(Leave `ModelAuthorizer: vm` on the same struct unchanged.)

- [ ] **Step 4: Build**

Run: `go build ./...`
Expected: succeeds with no errors.

- [ ] **Step 5: Run the app package tests**

Run: `go test ./internal/app/... -v`
Expected: PASS — `task_routing.enabled` defaults to `false`, so `taskrouting.New` returns `vm` unchanged and every existing `internal/app` test (which doesn't set `TASK_ROUTING_ENABLED`) sees identical behavior to before this change.

- [ ] **Step 6: Manually verify the default-off behavior end-to-end**

Run: `go run ./cmd/smartrouter --version` (sanity check the binary still builds/runs; no config file or API keys needed for `--version`)
Expected: prints the version and exits 0, confirming `app.New`'s wiring change didn't break startup.

- [ ] **Step 7: Commit**

```bash
git add internal/app/app.go
git commit -m "app: wire TaskRouting P0 resolver into inference and batch ModelResolver

Wraps virtualmodels.Service with taskrouting.New before assigning
ModelResolver on both the chat/embeddings orchestrator config and the
internal guardrails executor config. Disabled by default (task_routing.enabled
defaults to false), so this is a no-op until explicitly turned on."
```

---

### Task 8: Full-repo verification

**Files:** none (verification only).

- [ ] **Step 1: Full build**

Run: `go build ./...`
Expected: succeeds with no errors.

- [ ] **Step 2: Full build with the swagger tag** (per `CLAUDE.md`, this repo has a second build configuration)

Run: `go build -tags=swagger ./...`
Expected: succeeds with no errors.

- [ ] **Step 3: Full vet**

Run: `go vet ./...`
Expected: only the 3 pre-existing warnings in `internal/core/responses.go`/`types.go` (duplicate `json:"content"` struct tags, inherited from the original GoModel source, unrelated to this work). No new warnings.

- [ ] **Step 4: Full test suite**

Run: `go test ./...`
Expected: every package passes, including all new/modified packages from Tasks 1–7 (`internal/core`, `internal/gateway`, `config`, `internal/taskrouting`, `internal/app`).

- [ ] **Step 5: Spot-check that `task_routing.enabled=false` truly changes nothing**

Run: `go test ./internal/app/... ./internal/gateway/... ./internal/server/... -v -count=1`
Expected: PASS with `-count=1` (disables Go's test cache, forcing a real re-run) — confirms the default-off state hasn't silently altered behavior anywhere in the request path.

- [ ] **Step 6: Report status**

No commit for this task (verification only) — if any step fails, return to the task that introduced the regression, fix it there, and re-run this task from Step 1.

---

## Summary of what P0 does and does not include

Matches `docs/superpowers/specs/2026-07-27-taskrouting-design.md` exactly:

- **Included:** rule-based classifier (8 task types), `Resolver` wrapping `virtualmodels.Service`, `task_routing` config section, app wiring, classification-result labels reaching `usage.UsageEntry.Labels` (so `GET /admin/usage/labels` can already show task-routing hit rate once operators configure `task_routing.enabled: true` and declare the referenced virtual models).
- **Not included (future specs):** §7 dynamic candidate-pool/scoring algorithm, LLM-based classifier (P2), Dashboard task-routing visualization (P3), per-index env var explosion for `entrypoints`/`mappings`.
