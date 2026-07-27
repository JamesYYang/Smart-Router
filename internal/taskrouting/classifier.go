package taskrouting

import "context"

// Classifier decides a TaskType for one chat/responses request. The P0 MVP
// ships one implementation, RuleClassifier; the interface exists so a future
// LLM-based classifier (or a rule+LLM chain) can be swapped in later without
// changing Resolver.
type Classifier interface {
	Classify(ctx context.Context, input ClassificationInput) (Classification, error)
}
