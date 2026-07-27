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
