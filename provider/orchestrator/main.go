// Command orchestrator is the WASM_AF orchestrator capability provider.
// It connects to the wasmCloud lattice, exposes an HTTP task submission API,
// evaluates policy, manages agent component lifecycles, and drives the
// plan → dispatch → collect → iterate agent loop.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	natsjetstream "github.com/nats-io/nats.go/jetstream"
	wasmcloudprovider "go.wasmcloud.dev/provider"

	"github.com/jolucas/wasm-af/pkg/controlplane"
	"github.com/jolucas/wasm-af/pkg/taskstate"
)

const (
	listenAddr     = ":8080"
	defaultLattice = "default"
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
	// orch is built up incrementally as provider links are established.
	orch := &Orchestrator{
		logger:       logger,
		agentRefs:    make(map[string]string),
		allowedHosts: make(map[string]string),
	}

	// Initialise the wasmCloud provider SDK.
	// It reads host data from stdin, connects to the lattice NATS, and handles
	// link lifecycle messages (link put/del → wires up provider/component references).
	wasmProvider, err := wasmcloudprovider.New(
		wasmcloudprovider.HealthCheck(func() string { return "orchestrator healthy" }),
		wasmcloudprovider.TargetLinkPut(func(link wasmcloudprovider.InterfaceLinkDefinition) error {
			orch.initFromLinkConfig(link)
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

	// Populate static config from the WADM-provided named config map.
	orch.initFromHostConfig(wasmProvider.HostData().Config)

	// Re-use the provider's NATS connection for JetStream and control plane calls.
	nc := wasmProvider.NatsConnection()
	orch.nats = nc

	ctx := context.Background()

	js, err := natsjetstream.New(nc)
	if err != nil {
		return fmt.Errorf("jetstream: %w", err)
	}
	orch.js = js

	store, err := taskstate.NewStore(ctx, js)
	if err != nil {
		return fmt.Errorf("task store: %w", err)
	}
	orch.store = store

	lattice := wasmProvider.HostData().LatticeRPCPrefix
	if lattice == "" {
		lattice = defaultLattice
	}
	orch.lattice = lattice
	orch.ctl = controlplane.NewClient(nc, lattice)

	// HTTP server runs in background; provider.Start() blocks below.
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
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		logger.Info("HTTP server listening", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server error", "err", err)
		}
	}()

	// provider.Start() blocks until the lattice sends a shutdown signal.
	if err := wasmProvider.Start(); err != nil {
		return fmt.Errorf("provider: %w", err)
	}

	// Graceful HTTP shutdown after the provider exits.
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}
