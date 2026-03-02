// Command orchestrator is the WASM_AF orchestrator.
// It embeds the Extism WASM runtime to instantiate agent plugins on demand,
// evaluates policy via OPA, manages the task lifecycle, and drives the
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
	"strconv"
	"strings"
	"syscall"
	"time"

	natsjetstream "github.com/nats-io/nats.go/jetstream"

	nats "github.com/nats-io/nats.go"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

const (
	defaultListenAddr = ":8080"
	defaultNatsURL    = "nats://127.0.0.1:4222"
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
	listenAddr := envOr("LISTEN_ADDR", defaultListenAddr)
	wasmDir := envOr("WASM_DIR", "./components/target/wasm32-unknown-unknown/release")
	natsURL := envOr("NATS_URL", defaultNatsURL)
	opaPolicyPath := os.Getenv("OPA_POLICY")
	opaDataPath := os.Getenv("OPA_DATA")

	pluginTimeoutSec := envOrInt("PLUGIN_TIMEOUT_SEC", 30)
	pluginMaxMemPages := envOrInt("PLUGIN_MAX_MEMORY_PAGES", 256)    // 256 pages = 16 MiB
	pluginMaxHTTPBytes := envOrInt64("PLUGIN_MAX_HTTP_BYTES", 4<<20) // 4 MiB

	agentRegistryJSON := os.Getenv("AGENT_REGISTRY")
	if agentRegistryJSON == "" {
		if path := os.Getenv("AGENT_REGISTRY_FILE"); path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read agent registry file: %w", err)
			}
			agentRegistryJSON = string(b)
		}
	}
	if agentRegistryJSON == "" {
		return fmt.Errorf("AGENT_REGISTRY or AGENT_REGISTRY_FILE is required")
	}
	registry, err := ParseAgentRegistry([]byte(agentRegistryJSON))
	if err != nil {
		return fmt.Errorf("agent registry: %w", err)
	}
	logger.Info("loaded agent registry")

	planBuilders := NewPlanBuilderRegistry()
	RegisterDefaultBuilders(planBuilders)
	planBuilders.Register("chat", ChatBuilder{})
	planBuilders.Register("email-reply", EmailReplyBuilder{})
	planBuilders.Register("reply-all", ReplyAllBuilder{})
	planBuilders.Register("skill-demo", SkillDemoBuilder{})

	// ── HOST FUNCTION REGISTRY ──────────────────────────────────────────────
	hostFns := NewHostFnRegistry(logger)

	llmTimeoutSec := envOrInt("LLM_TIMEOUT_SEC", 120)
	llmCfg := LLMConfig{
		Mode:    envOr("LLM_MODE", "mock"),
		BaseURL: envOr("LLM_BASE_URL", ""),
		APIKey:  envOr("LLM_API_KEY", ""),
		Model:   envOr("LLM_MODEL", "gpt-4o-mini"),
		Timeout: time.Duration(llmTimeoutSec) * time.Second,
	}
	if v := os.Getenv("LLM_TEMPERATURE"); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			t := float32(f)
			llmCfg.Temperature = &t
		}
	}
	if v := os.Getenv("LLM_TOP_P"); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			t := float32(f)
			llmCfg.TopP = &t
		}
	}
	if llmCfg.Mode == "api" && llmCfg.APIKey == "" {
		return fmt.Errorf("LLM_MODE=api requires LLM_API_KEY to be set")
	}
	logger.Info("LLM configured", "mode", llmCfg.Mode, "model", llmCfg.Model)
	hostFns.Register("llm_complete", NewLLMHostFnProvider(llmCfg, logger))

	// ── OPA POLICY + DATA ────────────────────────────────────────────────────
	var initialData map[string]any
	if opaDataPath != "" {
		initialData, err = LoadDataFile(opaDataPath)
		if err != nil {
			return fmt.Errorf("load OPA data from %s: %w", opaDataPath, err)
		}
		logger.Info("OPA data loaded", "path", opaDataPath)
	}

	if opaPolicyPath == "" {
		return fmt.Errorf("OPA_POLICY is required: policy evaluation gates all agent execution")
	}
	modules, err := LoadRegoModules(opaPolicyPath)
	if err != nil {
		return fmt.Errorf("load rego policies from %s: %w", opaPolicyPath, err)
	}
	policy, err := NewOPAEvaluator(context.Background(), modules, initialData)
	if err != nil {
		return fmt.Errorf("init OPA evaluator: %w", err)
	}
	logger.Info("OPA policy loaded", "path", opaPolicyPath, "modules", len(modules))

	// Shell host functions — load config from env.
	shellAllowedCmds := strings.Split(envOr("SHELL_ALLOWED_COMMANDS", "ls,cat,pwd,echo,find,date,uname,wc,head,tail"), ",")
	shellAllowedPaths := strings.Split(envOr("SHELL_ALLOWED_PATHS", "/tmp/wasmclaw"), ",")
	shellTimeoutSec := envOrInt("SHELL_TIMEOUT_SEC", 10)
	hostFns.Register("exec_command", NewShellHostFnProvider(shellAllowedCmds, shellAllowedPaths, time.Duration(shellTimeoutSec)*time.Second, logger))

	// Sandbox execution — runs code inside WASM (Wazero), not on the host.
	sandboxRuntimesDir := envOr("SANDBOX_RUNTIMES_DIR", "./runtimes")
	sandboxTimeoutSec := envOrInt("SANDBOX_TIMEOUT_SEC", 30)
	if info, statErr := os.Stat(sandboxRuntimesDir); statErr == nil && info.IsDir() {
		sandboxEngine, sandboxErr := NewSandboxEngine(
			context.Background(), sandboxRuntimesDir,
			time.Duration(sandboxTimeoutSec)*time.Second, logger,
		)
		if sandboxErr != nil {
			return fmt.Errorf("sandbox engine: %w", sandboxErr)
		}
		defer sandboxEngine.Close(context.Background())

		sandboxLangs := make(map[string]bool)
		for _, l := range strings.Split(envOr("SANDBOX_ALLOWED_LANGUAGES", "python"), ",") {
			if l = strings.TrimSpace(l); l != "" {
				sandboxLangs[l] = true
			}
		}
		sandboxPaths := make(map[string]string)
		for _, p := range strings.Split(envOr("SANDBOX_ALLOWED_PATHS", "/tmp/wasmclaw"), ",") {
			if p = strings.TrimSpace(p); p != "" {
				sandboxPaths[p] = p
			}
		}
		hostFns.Register("sandbox_exec", NewSandboxHostFnProvider(sandboxEngine, sandboxLangs, sandboxPaths, logger))
		logger.Info("sandbox execution enabled", "runtimes_dir", sandboxRuntimesDir)
	} else {
		logger.Info("sandbox execution disabled (SANDBOX_RUNTIMES_DIR not found)", "path", sandboxRuntimesDir)
	}

	nc, err := nats.Connect(natsURL)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

	// Email send — SMTP credentials would be captured here in production.
	emailAllowedDomains := make(map[string]bool)
	for _, d := range strings.Split(envOr("EMAIL_ALLOWED_DOMAINS", "example.com,partner-corp.com"), ",") {
		if d = strings.TrimSpace(d); d != "" {
			emailAllowedDomains[d] = true
		}
	}
	hostFns.Register("send_email", NewEmailSendHostFnProvider(emailAllowedDomains, logger))

	// Memory host functions — require live NATS connection.
	kvGetProvider, kvPutProvider := NewMemoryHostFnProviders(nc, logger)
	hostFns.Register("kv_get", kvGetProvider)
	hostFns.Register("kv_put", kvPutProvider)

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

	approvalWebhookURL := os.Getenv("APPROVAL_WEBHOOK_URL")
	approvalTimeoutSec := envOrInt("APPROVAL_TIMEOUT_SEC", 0)

	orch := &Orchestrator{
		logger:               logger,
		store:                store,
		wasmDir:              wasmDir,
		policy:               policy,
		registry:             registry,
		builders:             planBuilders,
		hostFns:              hostFns,
		ctx:                  ctx,
		pluginTimeout:        time.Duration(pluginTimeoutSec) * time.Second,
		pluginMaxMemoryPages: uint32(pluginMaxMemPages),
		pluginMaxHTTPBytes:   pluginMaxHTTPBytes,
		natsConn:             nc,
		configKV:             configKV,
		approvalWebhookURL:   approvalWebhookURL,
		approvalTimeoutSec:   approvalTimeoutSec,
	}

	if approvalWebhookURL != "" {
		logger.Info("approval webhook configured", "url", approvalWebhookURL)
	}
	if approvalTimeoutSec > 0 {
		logger.Info("approval timeout configured", "timeout_sec", approvalTimeoutSec)
	}

	// Validate that agent registry host_functions reference registered providers.
	for name, meta := range registry.List() {
		for _, hf := range meta.HostFunctions {
			if !hostFns.Has(hf) {
				logger.Warn("agent references unregistered host function",
					"agent", name, "host_function", hf)
			}
		}
	}

	// Synchronously load the current allowlist from NATS KV into OPA data store.
	if policy != nil {
		if entry, err := configKV.Get(ctx, "allowed-fetch-domains"); err == nil {
			domains := parseDomainCSV(string(entry.Value()))
			if err := policy.UpdateData(ctx, "/config/allowed_domains", domains); err != nil {
				logger.Error("failed to seed allowed_domains in OPA", "err", err)
			} else {
				logger.Info("loaded allowed-fetch-domains from KV into OPA", "count", len(domains))
			}
		}
	}

	// Watch for live updates — pushes domain allowlist changes into OPA data store.
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
					continue
				}
				if policy == nil {
					continue
				}
				switch update.Operation() {
				case natsjetstream.KeyValueDelete, natsjetstream.KeyValuePurge:
					if err := policy.UpdateData(ctx, "/config/allowed_domains", []string{}); err != nil {
						logger.Error("failed to clear allowed_domains in OPA", "err", err)
					} else {
						logger.Info("allowed-fetch-domains cleared in OPA — no domain restrictions")
					}
				default:
					domains := parseDomainCSV(string(update.Value()))
					if err := policy.UpdateData(ctx, "/config/allowed_domains", domains); err != nil {
						logger.Error("failed to update allowed_domains in OPA", "err", err)
					} else {
						logger.Info("allowed-fetch-domains updated in OPA", "count", len(domains))
					}
				}
			}
		}
	}()

	// ── BYOA: seed external agents from NATS KV ────────────────────────────
	if entry, err := configKV.Get(ctx, "external-agents"); err == nil {
		if n, seedErr := seedExternalAgents(registry, entry.Value()); seedErr != nil {
			logger.Error("failed to seed external agents from KV", "err", seedErr)
		} else if n > 0 {
			logger.Info("seeded external agents from KV", "count", n)
		}
	}

	// Watch for live updates to the external agent registry.
	go func() {
		watcher, err := configKV.Watch(ctx, "external-agents")
		if err != nil {
			logger.Error("failed to start external-agents watcher", "err", err)
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
					continue
				}
				switch update.Operation() {
				case natsjetstream.KeyValueDelete, natsjetstream.KeyValuePurge:
					clearExternalAgents(registry)
					logger.Info("external agents cleared from registry")
				default:
					if n, err := seedExternalAgents(registry, update.Value()); err != nil {
						logger.Error("failed to sync external agents from KV", "err", err)
					} else {
						logger.Info("external agents synced from KV", "count", n)
					}
				}
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /tasks", orch.handleSubmitTask)
	mux.HandleFunc("GET /tasks/{id}", orch.handleGetTask)
	mux.HandleFunc("GET /tasks/{id}/approvals", orch.handleListApprovals)
	mux.HandleFunc("POST /tasks/{id}/steps/{stepId}/approve", orch.handleApproveStep)
	mux.HandleFunc("POST /tasks/{id}/steps/{stepId}/reject", orch.handleRejectStep)
	mux.HandleFunc("POST /agents", orch.handleRegisterAgent)
	mux.HandleFunc("DELETE /agents/{name}", orch.handleRemoveAgent)
	mux.HandleFunc("GET /agents", orch.handleListAgents)
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
	cancel()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}

func parseDomainCSV(csv string) []string {
	var domains []string
	for _, d := range strings.Split(csv, ",") {
		if d = strings.TrimSpace(d); d != "" {
			domains = append(domains, d)
		}
	}
	return domains
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func envOrInt64(key string, fallback int64) int64 {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return fallback
}
