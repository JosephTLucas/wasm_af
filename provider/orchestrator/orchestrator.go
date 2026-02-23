package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"

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

	llmMode    string // "mock" or "real"
	llmBaseURL string
	llmAPIKey  string
	llmModel   string
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
	CommsMode   *string `json:"comms_mode"`
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
	}
	if len(allowedHosts) > 0 {
		manifest.AllowedHosts = allowedHosts
	}

	config := extism.PluginConfig{
		EnableWasi: true,
	}

	plugin, err := extism.NewPlugin(ctx, manifest, config, hostFunctions)
	if err != nil {
		return nil, fmt.Errorf("create plugin %s: %w", wasmName, err)
	}
	defer plugin.Close(ctx)

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
