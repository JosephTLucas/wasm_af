package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"time"

	extism "github.com/extism/go-sdk"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// Orchestrator is the central coordinator. It embeds the Extism WASM runtime
// and manages task state via NATS JetStream KV. The struct is agent-agnostic —
// all agent-specific behavior lives in the registry, host function providers,
// and Rego policies.
type Orchestrator struct {
	logger   *slog.Logger
	store    *taskstate.Store
	wasmDir  string // directory containing compiled .wasm plugins
	policy   *OPAEvaluator
	registry *AgentRegistry
	builders *PlanBuilderRegistry
	hostFns  *HostFnRegistry

	ctx context.Context

	pluginTimeout        time.Duration
	pluginMaxMemoryPages uint32
	pluginMaxHTTPBytes   int64
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
// the plugin is instantiated (resource limits, network scoping, config,
// filesystem access, host function filtering).
type PolicyResult struct {
	Permitted     bool              `json:"permitted"`
	DenyCode      *string           `json:"deny_code,omitempty"`
	DenyMessage   *string           `json:"deny_message,omitempty"`
	AllowedHosts  []string          `json:"allowed_hosts,omitempty"`
	MaxMemPages   *uint32           `json:"max_memory_pages,omitempty"`
	MaxHTTPBytes  *int64            `json:"max_http_bytes,omitempty"`
	TimeoutSec    *int              `json:"timeout_sec,omitempty"`
	HostFunctions []string          `json:"host_functions,omitempty"`
	Config        map[string]string `json:"config,omitempty"`
	AllowedPaths  map[string]string `json:"allowed_paths,omitempty"`
}

// PluginOpts carries per-step overrides for plugin creation,
// potentially set by policy decisions.
type PluginOpts struct {
	AllowedHosts  []string
	HostFunctions []extism.HostFunction
	MaxMemPages   uint32
	MaxHTTPBytes  int64
	Timeout       time.Duration
	Config        map[string]string
	AllowedPaths  map[string]string
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
	if len(opts.Config) > 0 {
		manifest.Config = opts.Config
	}
	if len(opts.AllowedPaths) > 0 {
		manifest.AllowedPaths = opts.AllowedPaths
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

	enrichedParams := enrichParams(step.Params, meta.Enrichments)

	input := map[string]any{
		"step": map[string]any{
			"id":         step.ID,
			"index":      stepIdx,
			"agent_type": step.AgentType,
			"group":      step.Group,
			"params":     enrichedParams,
			"domain":     enrichedParams["domain"],
		},
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

// enrichParams applies the declared enrichments to a copy of the step params.
// Transforms are generic and driven by the agent registry JSON, not hardcoded.
func enrichParams(params map[string]string, enrichments []ParamEnrichment) map[string]string {
	out := make(map[string]string, len(params))
	for k, v := range params {
		out[k] = v
	}
	for _, e := range enrichments {
		src, ok := out[e.Source]
		if !ok {
			continue
		}
		switch e.Transform {
		case "domain":
			out[e.Target] = extractDomain(src)
		default:
			out[e.Target] = src
		}
	}
	return out
}

func extractDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return rawURL
	}
	return u.Hostname()
}
