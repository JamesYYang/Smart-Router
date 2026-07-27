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
