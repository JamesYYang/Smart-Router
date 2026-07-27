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
