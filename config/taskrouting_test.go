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
