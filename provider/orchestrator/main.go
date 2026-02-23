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

	nc, err := nats.Connect(natsURL)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

	js, err := natsjetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}

	ctx := context.Background()
	store, err := taskstate.NewStore(ctx, js)
	if err != nil {
		return fmt.Errorf("task store: %w", err)
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

	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
