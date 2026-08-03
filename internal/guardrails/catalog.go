package guardrails

import "context"

// Catalog resolves named guardrail references into executable pipelines.
// BuildPipeline returns the compiled pipeline, a deterministic configuration hash
// for cache/change detection, and an error. The hash should change whenever the
// effective pipeline configuration changes; empty is reserved for "no pipeline".
// The context selects the tenant whose guardrails are resolved (falling back to
// the platform-default tenant when it carries no tenant ID).
type Catalog interface {
	Len() int
	Names() []string
	BuildPipeline(ctx context.Context, steps []StepReference) (*Pipeline, string, error)
}
