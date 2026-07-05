package run

import (
	"smartrouter/config"
	"smartrouter/internal/observability"
	"smartrouter/internal/providers"
	"smartrouter/internal/providers/anthropic"
	"smartrouter/internal/providers/azure"
	"smartrouter/internal/providers/bailian"
	"smartrouter/internal/providers/bedrock"
	"smartrouter/internal/providers/deepseek"
	"smartrouter/internal/providers/fireworks"
	"smartrouter/internal/providers/gemini"
	"smartrouter/internal/providers/groq"
	"smartrouter/internal/providers/minimax"
	"smartrouter/internal/providers/ollama"
	"smartrouter/internal/providers/openai"
	"smartrouter/internal/providers/opencodego"
	"smartrouter/internal/providers/openrouter"
	"smartrouter/internal/providers/oracle"
	"smartrouter/internal/providers/vertex"
	"smartrouter/internal/providers/vllm"
	"smartrouter/internal/providers/xai"
	"smartrouter/internal/providers/xiaomi"
	"smartrouter/internal/providers/zai"
)

// defaultProviderFactory builds the provider factory with every provider type
// the standard gateway ships with.
func defaultProviderFactory(cfg *config.Config) *providers.ProviderFactory {
	factory := providers.NewProviderFactory()

	if cfg.Metrics.Enabled {
		factory.SetHooks(observability.NewPrometheusHooks())
	}

	factory.Add(openai.Registration)
	factory.Add(openrouter.Registration)
	factory.Add(azure.Registration)
	factory.Add(bailian.Registration)
	factory.Add(oracle.Registration)
	factory.Add(anthropic.Registration)
	factory.Add(bedrock.Registration)
	factory.Add(deepseek.Registration)
	factory.Add(fireworks.Registration)
	factory.Add(gemini.Registration)
	factory.Add(vertex.Registration)
	factory.Add(groq.Registration)
	factory.Add(minimax.Registration)
	factory.Add(ollama.Registration)
	factory.Add(opencodego.Registration)
	factory.Add(vllm.Registration)
	factory.Add(xai.Registration)
	factory.Add(xiaomi.Registration)
	factory.Add(zai.Registration)

	return factory
}
