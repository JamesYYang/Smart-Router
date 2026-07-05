package config

import "time"

// WorkflowsConfig holds runtime refresh behavior for persisted workflows.
type WorkflowsConfig struct {
	// RefreshInterval controls how often the in-memory workflow snapshot
	// is refreshed from storage. Default: 1m.
	RefreshInterval time.Duration `yaml:"refresh_interval" env:"WORKFLOW_REFRESH_INTERVAL"`
}
