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
func (r *Resolver) ResolveModel(ctx context.Context, requested core.RequestedModelSelector) (core.ModelSelector, bool, error) {
	selector, changed, _, err := r.resolveModel(ctx, requested)
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
	return r.next.ResolveModel(ctx, requested)
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
