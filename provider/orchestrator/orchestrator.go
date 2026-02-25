package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// Orchestrator is the central coordinator. It embeds the Extism WASM runtime
// and manages task state via NATS JetStream KV.
type Orchestrator struct {
	logger          *slog.Logger
	store           *taskstate.Store
	wasmDir         string // directory containing compiled .wasm plugins
	policyRulesJSON string // JSON policy rules passed to the policy engine plugin
	registry        *AgentRegistry
	builders        *PlanBuilderRegistry

	// ctx is the server-lifetime context, cancelled on SIGINT/SIGTERM.
	// Task goroutines derive from this so in-flight work stops on shutdown.
	ctx context.Context

	llmMode    string // "mock" or "real"
	llmBaseURL string
	llmAPIKey  string
	llmModel   string

	// pluginTimeout is the maximum wall-clock time a single plugin invocation
	// may run before the context is cancelled and the WASM execution is aborted.
	pluginTimeout time.Duration
	// pluginMaxMemoryPages caps the linear memory a plugin instance may allocate.
	// One page = 64 KiB. 0 means no limit (not recommended in production).
	pluginMaxMemoryPages uint32
	// pluginMaxHTTPBytes caps the size of any HTTP response a plugin may read.
	pluginMaxHTTPBytes int64

	// allowedFetchDomains is the server-side domain allowlist for url-fetch steps.
	// Stored in NATS KV (wasm-af-config / allowed-fetch-domains) and kept in sync
	// by a live watcher. When non-empty, any url-fetch step whose domain is absent
	// is denied at plan-build time — before a plugin is instantiated. Empty means
	// no restriction (dev/open mode). All access must go through fetchDomainAllowed.
	allowedFetchMu      sync.RWMutex
	allowedFetchDomains map[string]bool
}

// setAllowedFetchDomains replaces the in-memory allowlist from a
// comma-separated string. Called at startup and on every KV update.
func (o *Orchestrator) setAllowedFetchDomains(csv string) {
	domains := map[string]bool{}
	for _, d := range strings.Split(csv, ",") {
		if d = strings.TrimSpace(d); d != "" {
			domains[d] = true
		}
	}
	o.allowedFetchMu.Lock()
	o.allowedFetchDomains = domains
	o.allowedFetchMu.Unlock()
}

// fetchDomainAllowed reports whether domain is permitted for url-fetch steps.
// Returns true when no allowlist is configured (open/dev mode).
func (o *Orchestrator) fetchDomainAllowed(domain string) bool {
	o.allowedFetchMu.RLock()
	defer o.allowedFetchMu.RUnlock()
	if len(o.allowedFetchDomains) == 0 {
		return true
	}
	return o.allowedFetchDomains[domain]
}

// TaskInput is the JSON structure passed to every agent plugin.
type TaskInput struct {
	TaskID  string   `json:"task_id"`
	StepID  string   `json:"step_id"`
	Payload string   `json:"payload"`
	Context []KVPair `json:"context"`
}

type KVPair struct {
	Key string `json:"key"`
	Val string `json:"val"`
}

// TaskOutput is the JSON structure returned by every agent plugin.
type TaskOutput struct {
	Payload  string   `json:"payload"`
	Metadata []KVPair `json:"metadata"`
}

// PolicyRequest is the JSON input to the policy engine plugin.
type PolicyRequest struct {
	Source     string `json:"source"`
	Target     string `json:"target"`
	Capability string `json:"capability"`
	TaskID     string `json:"task_id"`
}

// PolicyResult is the JSON output from the policy engine plugin.
type PolicyResult struct {
	Permitted   bool    `json:"permitted"`
	DenyCode    *string `json:"deny_code"`
	DenyMessage *string `json:"deny_message"`
}

// wasmPath returns the absolute path to a compiled .wasm plugin.
func (o *Orchestrator) wasmPath(name string) string {
	return filepath.Join(o.wasmDir, name+".wasm")
}

// invokeAgent creates an Extism plugin from the given WASM binary, calls
// its "execute" export with the provided TaskInput, and returns the TaskOutput.
// The plugin is created with the given allowed_hosts (for HTTP scoping) and
// host functions (for LLM access). The plugin instance is destroyed when
// this function returns.
func (o *Orchestrator) invokeAgent(
	ctx context.Context,
	wasmName string,
	input *TaskInput,
	allowedHosts []string,
	hostFunctions []extism.HostFunction,
) (*TaskOutput, error) {
	manifest := extism.Manifest{
		Wasm: []extism.Wasm{
			extism.WasmFile{Path: o.wasmPath(wasmName)},
		},
		Memory: &extism.ManifestMemory{
			MaxPages:             o.pluginMaxMemoryPages,
			MaxHttpResponseBytes: o.pluginMaxHTTPBytes,
		},
	}
	if len(allowedHosts) > 0 {
		manifest.AllowedHosts = allowedHosts
	}

	// Extism PDK uses WASI for fd_write (logging) and clock_time_get.
	config := extism.PluginConfig{
		EnableWasi: true,
	}

	plugin, err := extism.NewPlugin(ctx, manifest, config, hostFunctions)
	if err != nil {
		return nil, fmt.Errorf("create plugin %s: %w", wasmName, err)
	}
	defer plugin.Close(ctx)

	if o.pluginTimeout > 0 {
		plugin.Timeout = o.pluginTimeout
	}

	inputJSON, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("marshal input: %w", err)
	}

	_, outputJSON, err := plugin.Call("execute", inputJSON)
	if err != nil {
		return nil, fmt.Errorf("call execute on %s: %w", wasmName, err)
	}

	var output TaskOutput
	if err := json.Unmarshal(outputJSON, &output); err != nil {
		return nil, fmt.Errorf("unmarshal output from %s: %w", wasmName, err)
	}

	return &output, nil
}

// evaluatePolicy creates an Extism policy-engine plugin, calls its "evaluate"
// export, and returns whether the request is permitted.
func (o *Orchestrator) evaluatePolicy(
	ctx context.Context,
	taskID string,
	stepID string,
	source, target, capability string,
) (*PolicyResult, error) {
	manifest := extism.Manifest{
		Wasm: []extism.Wasm{
			extism.WasmFile{Path: o.wasmPath("policy_engine")},
		},
		Config: map[string]string{
			"policy-rules": o.policyRulesJSON,
		},
		Memory: &extism.ManifestMemory{
			MaxPages: o.pluginMaxMemoryPages,
		},
	}

	config := extism.PluginConfig{}
	plugin, err := extism.NewPlugin(ctx, manifest, config, nil)
	if err != nil {
		return nil, fmt.Errorf("create policy plugin: %w", err)
	}
	defer plugin.Close(ctx)

	req := PolicyRequest{
		Source:     source,
		Target:     target,
		Capability: capability,
		TaskID:     taskID,
	}

	inputJSON, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal policy request: %w", err)
	}

	_, outputJSON, err := plugin.Call("evaluate", inputJSON)
	if err != nil {
		return nil, fmt.Errorf("call evaluate: %w", err)
	}

	var result PolicyResult
	if err := json.Unmarshal(outputJSON, &result); err != nil {
		return nil, fmt.Errorf("unmarshal policy result: %w", err)
	}

	return &result, nil
}
