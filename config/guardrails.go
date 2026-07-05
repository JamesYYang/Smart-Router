package config

// GuardrailsConfig holds configuration for the request guardrails pipeline.
type GuardrailsConfig struct {
	// Enabled controls whether guardrails are active
	// Default: false
	Enabled bool `yaml:"enabled" env:"GUARDRAILS_ENABLED"`

	// EnableForBatchProcessing controls whether guardrails are applied to inline
	// batch items for /v1/batches requests.
	// Default: false
	EnableForBatchProcessing bool `yaml:"enable_for_batch_processing" env:"ENABLE_GUARDRAILS_FOR_BATCH_PROCESSING"`

	// Rules is a list of guardrail instances. Each entry defines one guardrail
	// with its own name, type, order, and type-specific settings. Multiple
	// instances of the same type are allowed (e.g. two system_prompt guardrails
	// with different content).
	Rules []GuardrailRuleConfig `yaml:"rules"`
}

// GuardrailRuleConfig defines a single guardrail instance.
type GuardrailRuleConfig struct {
	// Name is a unique identifier for this guardrail instance (used in logs and errors)
	Name string `yaml:"name"`

	// Type selects the guardrail implementation: "system_prompt" or "llm_based_altering"
	Type string `yaml:"type"`

	// UserPath scopes internal auxiliary guardrail requests for workflow
	// selection and audit logging. When empty, the caller user path is used.
	UserPath string `yaml:"user_path"`

	// Order controls execution ordering relative to other guardrails.
	// Guardrails with the same order run in parallel; different orders run sequentially.
	// Default: 0
	Order int `yaml:"order"`

	// SystemPrompt holds settings when Type is "system_prompt"
	SystemPrompt SystemPromptSettings `yaml:"system_prompt"`

	// LLMBasedAltering holds settings when Type is "llm_based_altering"
	LLMBasedAltering LLMBasedAlteringSettings `yaml:"llm_based_altering"`
}

// SystemPromptSettings holds the type-specific settings for a system_prompt guardrail.
type SystemPromptSettings struct {
	// Mode controls how the system prompt is applied: "inject", "override", or "decorator"
	//   - inject: adds a system message only if none exists
	//   - override: replaces all existing system messages
	//   - decorator: prepends to the first existing system message
	// Default: "inject"
	Mode string `yaml:"mode"`

	// Content is the system prompt text to apply
	Content string `yaml:"content"`
}

// LLMBasedAlteringSettings holds the type-specific settings for an llm_based_altering guardrail.
type LLMBasedAlteringSettings struct {
	// Model is the model selector used for the auxiliary rewrite call.
	// This can be a concrete model name, provider-qualified selector, or alias.
	Model string `yaml:"model"`

	// Provider is an optional routing hint for Model.
	Provider string `yaml:"provider"`

	// Prompt is the system prompt used to rewrite targeted messages.
	// When empty, the built-in LiteLLM-derived anonymization prompt is used.
	Prompt string `yaml:"prompt"`

	// Roles selects which message roles are rewritten.
	// Default: ["user"]
	Roles []string `yaml:"roles"`

	// SkipContentPrefix skips rewriting for messages whose trimmed text begins with this prefix.
	SkipContentPrefix string `yaml:"skip_content_prefix"`

	// MaxTokens limits the auxiliary rewrite completion.
	// Default: 4096
	MaxTokens int `yaml:"max_tokens"`
}
