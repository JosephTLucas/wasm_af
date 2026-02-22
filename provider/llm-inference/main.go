// Command llm-inference is the WASM_AF LLM capability provider.
// It implements the wasm-af:llm/inference.complete WIT interface over wRPC,
// routing requests to an OpenAI-compatible HTTP API endpoint.
// The base URL and API key are delivered via wasmCloud link secrets,
// scoped per agent link — no agent can reach another agent's credentials.
package main

import (
	"fmt"
	"log/slog"
	"os"

	wasmcloudprovider "go.wasmcloud.dev/provider"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(logger); err != nil {
		logger.Error("llm-inference exited with error", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	router := newRouter(logger)

	wasmProvider, err := wasmcloudprovider.New(
		wasmcloudprovider.HealthCheck(func() string { return "llm-inference healthy" }),

		// When a component links to this provider (source link), extract the
		// endpoint and API key from the link secrets and register a per-link route.
		wasmcloudprovider.SourceLinkPut(func(link wasmcloudprovider.InterfaceLinkDefinition) error {
			baseURL := link.SourceConfig["llm_base_url"]
			model := link.SourceConfig["llm_default_model"]

			apiKey := ""
			if sk, ok := link.SourceSecrets["llm_api_key"]; ok {
				apiKey = sk.String.Reveal()
			}

			if baseURL == "" {
				logger.Warn("no llm_base_url in link config; using default OpenAI endpoint",
					"source_id", link.SourceID)
				baseURL = "https://api.openai.com"
			}
			if model == "" {
				model = "gpt-4o"
			}

			router.registerRoute(link.SourceID, endpointConfig{
				BaseURL:      baseURL,
				APIKey:       apiKey,
				DefaultModel: model,
			})
			logger.Info("registered LLM route",
				"source_id", link.SourceID,
				"base_url", baseURL,
				"default_model", model,
			)
			return nil
		}),

		// Remove the route when the link is torn down.
		wasmcloudprovider.SourceLinkDel(func(link wasmcloudprovider.InterfaceLinkDefinition) error {
			router.removeRoute(link.SourceID)
			logger.Info("removed LLM route", "source_id", link.SourceID)
			return nil
		}),

		wasmcloudprovider.Shutdown(func() error {
			logger.Info("shutdown requested by wasmCloud host")
			return nil
		}),
	)
	if err != nil {
		return fmt.Errorf("init provider: %w", err)
	}

	// Register the wRPC handler for wasm-af:llm/inference.complete
	// The wasmCloud host routes incoming wRPC messages to us via NATS.
	nc := wasmProvider.NatsConnection()
	if err := router.subscribeWRPC(nc, wasmProvider.HostData().ProviderKey); err != nil {
		return fmt.Errorf("subscribe wRPC: %w", err)
	}

	logger.Info("llm-inference provider started")
	return wasmProvider.Start()
}
