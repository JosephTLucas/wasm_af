// Command orchestrator is the WASM_AF orchestrator.
// It embeds the Extism WASM runtime to instantiate agent plugins on demand,
// evaluates policy, manages the task lifecycle, and drives the
// plan -> dispatch -> collect -> iterate agent loop.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	natsjetstream "github.com/nats-io/nats.go/jetstream"

	nats "github.com/nats-io/nats.go"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

const (
	listenAddr     = ":8080"
	defaultNatsURL = "nats://127.0.0.1:4222"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	if err := run(logger); err != nil {
		logger.Error("orchestrator exited with error", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	wasmDir := envOr("WASM_DIR", "./components/target/wasm32-unknown-unknown/release")
	natsURL := envOr("NATS_URL", defaultNatsURL)
	policyRulesJSON := os.Getenv("POLICY_RULES")
	if policyRulesJSON == "" {
		if path := os.Getenv("POLICY_RULES_FILE"); path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read policy rules file: %w", err)
			}
			policyRulesJSON = string(b)
		}
	}
	llmMode := envOr("LLM_MODE", "mock")
	llmBaseURL := envOr("LLM_BASE_URL", "")
	llmAPIKey := envOr("LLM_API_KEY", "")
	llmModel := envOr("LLM_MODEL", "gpt-4o-mini")

	// URL_FETCH_ALLOWED_DOMAINS seeds the NATS KV entry on first run.
	// After that, the live value in KV is authoritative and can be updated
	// without restarting: nats kv put wasm-af-config allowed-fetch-domains "a.com,b.com"
	seedDomains := os.Getenv("URL_FETCH_ALLOWED_DOMAINS")

	nc, err := nats.Connect(natsURL)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

	js, err := natsjetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := taskstate.NewStore(ctx, js)
	if err != nil {
		return fmt.Errorf("task store: %w", err)
	}

	// Config KV bucket — holds runtime-mutable orchestrator configuration.
	configKV, err := js.CreateOrUpdateKeyValue(ctx, natsjetstream.KeyValueConfig{
		Bucket:      "wasm-af-config",
		Description: "wasm-af orchestrator runtime configuration",
	})
	if err != nil {
		return fmt.Errorf("config KV: %w", err)
	}

	// Seed allowed-fetch-domains from env var on first run (key absent in KV).
	if seedDomains != "" {
		if _, err := configKV.Get(ctx, "allowed-fetch-domains"); err != nil {
			if _, putErr := configKV.Put(ctx, "allowed-fetch-domains", []byte(seedDomains)); putErr != nil {
				logger.Warn("failed to seed allowed-fetch-domains in KV", "err", putErr)
			} else {
				logger.Info("seeded allowed-fetch-domains from env", "domains", seedDomains)
			}
		}
	}

	orch := &Orchestrator{
		logger:          logger,
		store:           store,
		wasmDir:         wasmDir,
		policyRulesJSON: policyRulesJSON,
		llmMode:         llmMode,
		llmBaseURL:      llmBaseURL,
		llmAPIKey:       llmAPIKey,
		llmModel:        llmModel,
	}

	// Synchronously load the current allowlist before accepting requests.
	if entry, err := configKV.Get(ctx, "allowed-fetch-domains"); err == nil {
		orch.setAllowedFetchDomains(string(entry.Value()))
		logger.Info("loaded allowed-fetch-domains from KV", "domains", string(entry.Value()))
	}

	// Watch for live updates — no restart needed to change the allowlist.
	go func() {
		watcher, err := configKV.Watch(ctx, "allowed-fetch-domains")
		if err != nil {
			logger.Error("failed to start allowed-fetch-domains watcher", "err", err)
			return
		}
		defer watcher.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case update, ok := <-watcher.Updates():
				if !ok {
					return
				}
				if update == nil {
					// Marker: initial values fully delivered; now in live-watch mode.
					continue
				}
				switch update.Operation() {
				case natsjetstream.KeyValueDelete, natsjetstream.KeyValuePurge:
					orch.setAllowedFetchDomains("")
					logger.Info("allowed-fetch-domains cleared — no domain restrictions")
				default:
					csv := string(update.Value())
					orch.setAllowedFetchDomains(csv)
					logger.Info("allowed-fetch-domains updated", "domains", csv)
				}
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", orch.handleSubmitTask)
	mux.HandleFunc("GET /tasks/{id}", orch.handleGetTask)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{
		Addr:         listenAddr,
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 120 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		logger.Info("HTTP server listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server error", "err", err)
		}
	}()

	<-sigCh
	logger.Info("shutdown requested")
	cancel() // stop the KV watcher goroutine

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
