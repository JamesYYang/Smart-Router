package taskrouting

import (
	"context"
	"testing"

	"smartrouter/config"
	"smartrouter/internal/core"
)

type fakeNextResolver struct {
	calls int
}

func (f *fakeNextResolver) ResolveModel(_ context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
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
	if _, _, err := resolver.ResolveModel(context.Background(), core.NewRequestedModelSelector("openai/gpt-4o", "")); err != nil {
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
