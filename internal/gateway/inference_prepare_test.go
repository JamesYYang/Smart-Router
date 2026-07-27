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
