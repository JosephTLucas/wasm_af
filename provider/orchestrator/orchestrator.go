package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	extism "github.com/extism/go-sdk"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// Orchestrator is the central coordinator. It embeds the Extism WASM runtime
// and manages task state via NATS JetStream KV.
type Orchestrator struct {
	logger   *slog.Logger
	store    *taskstate.Store
	wasmDir  string // directory containing compiled .wasm plugins
	policy   *OPAEvaluator
	registry *AgentRegistry
	builders *PlanBuilderRegistry

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

// PolicyResult is the outcome of an OPA policy evaluation. Beyond the
// allow/deny decision, it may carry structured overrides that shape how
// the plugin is instantiated (resource limits, network scoping).
type PolicyResult struct {
	Permitted    bool     `json:"permitted"`
	DenyCode     *string  `json:"deny_code,omitempty"`
	DenyMessage  *string  `json:"deny_message,omitempty"`
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	MaxMemPages  *uint32  `json:"max_memory_pages,omitempty"`
	MaxHTTPBytes *int64   `json:"max_http_bytes,omitempty"`
	TimeoutSec   *int     `json:"timeout_sec,omitempty"`
}

// PluginOpts carries per-step overrides for plugin creation,
// potentially set by policy decisions.
type PluginOpts struct {
	AllowedHosts  []string
	HostFunctions []extism.HostFunction
	MaxMemPages   uint32
	MaxHTTPBytes  int64
	Timeout       time.Duration
}

// wasmPath returns the absolute path to a compiled .wasm plugin.
func (o *Orchestrator) wasmPath(name string) string {
	return filepath.Join(o.wasmDir, name+".wasm")
}

// invokeAgent creates an Extism plugin from the given WASM binary, calls
// its "execute" export with the provided TaskInput, and returns the TaskOutput.
// The plugin is created with the capabilities in opts. The plugin instance is
// destroyed when this function returns.
func (o *Orchestrator) invokeAgent(
	ctx context.Context,
	wasmName string,
	input *TaskInput,
	opts PluginOpts,
) (*TaskOutput, error) {
	manifest := extism.Manifest{
		Wasm: []extism.Wasm{
			extism.WasmFile{Path: o.wasmPath(wasmName)},
		},
		Memory: &extism.ManifestMemory{
			MaxPages:             opts.MaxMemPages,
			MaxHttpResponseBytes: opts.MaxHTTPBytes,
		},
	}
	if len(opts.AllowedHosts) > 0 {
		manifest.AllowedHosts = opts.AllowedHosts
	}

	config := extism.PluginConfig{
		EnableWasi: true,
	}

	plugin, err := extism.NewPlugin(ctx, manifest, config, opts.HostFunctions)
	if err != nil {
		return nil, fmt.Errorf("create plugin %s: %w", wasmName, err)
	}
	defer plugin.Close(ctx)

	if opts.Timeout > 0 {
		plugin.Timeout = opts.Timeout
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

// evaluateStepPolicy runs the wasm_af.authz policy with rich context
// about the task, step, agent, and plan. Returns deny-all when no
// policy is loaded.
func (o *Orchestrator) evaluateStepPolicy(
	ctx context.Context,
	state *taskstate.TaskState,
	step *taskstate.Step,
	meta *AgentMeta,
	stepIdx int,
) (*PolicyResult, error) {
	if o.policy == nil {
		return &PolicyResult{
			Permitted:   false,
			DenyCode:    strPtr("no-policy"),
			DenyMessage: strPtr("no OPA policy loaded; deny-all"),
		}, nil
	}

	stepInput := map[string]any{
		"id":         step.ID,
		"index":      stepIdx,
		"agent_type": step.AgentType,
		"group":      step.Group,
		"params":     step.Params,
	}
	if u, ok := step.Params["url"]; ok {
		stepInput["domain"] = extractDomain(u)
	}

	input := map[string]any{
		"step": stepInput,
		"agent": map[string]any{
			"wasm_name":      meta.WasmName,
			"capability":     meta.Capability,
			"host_functions": meta.HostFunctions,
		},
		"task": map[string]any{
			"id":         state.TaskID,
			"type":       state.Context["type"],
			"created_at": state.CreatedAt,
		},
		"plan": map[string]any{
			"total_steps":     len(state.Plan),
			"completed_steps": countCompleted(state),
		},
	}
	return o.policy.EvaluateStep(ctx, input)
}

// evaluateSubmitPolicy runs the wasm_af.submit policy for task submission.
// Defaults to allow when no policy is loaded.
func (o *Orchestrator) evaluateSubmitPolicy(ctx context.Context, taskType, query string, taskCtx map[string]string) (*PolicyResult, error) {
	if o.policy == nil {
		return &PolicyResult{Permitted: true}, nil
	}

	input := map[string]any{
		"task_type": taskType,
		"query":     query,
		"context":   taskCtx,
	}
	return o.policy.EvaluateSubmit(ctx, input)
}

func countCompleted(state *taskstate.TaskState) int {
	n := 0
	for _, s := range state.Plan {
		if s.Status == taskstate.StepCompleted {
			n++
		}
	}
	return n
}
