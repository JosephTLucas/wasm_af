package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// ParamEnrichment declares a derived parameter computed from an existing
// step param. This moves agent-specific knowledge (e.g. "extract domain
// from URL") out of the orchestrator Go code and into the agent registry JSON.
type ParamEnrichment struct {
	Source    string `json:"source"`    // param name to read from
	Target    string `json:"target"`    // enriched param name to write
	Transform string `json:"transform"` // transform to apply: "domain"
}

// AgentMeta describes a single agent type's metadata: how to load it,
// what capability it requires, what host functions it needs, how to
// build its input payload, and what param enrichments to apply.
type AgentMeta struct {
	WasmName      string            `json:"wasm_name"`
	Capability    string            `json:"capability"`
	ContextKey    string            `json:"context_key"`
	HostFunctions []string          `json:"host_functions"`
	PayloadFields map[string]any    `json:"payload_fields"`
	Enrichments   []ParamEnrichment `json:"enrichments,omitempty"`
}

// AgentRegistry holds the set of known agent types, loaded from a JSON config.
type AgentRegistry struct {
	agents map[string]*AgentMeta
}

// LoadAgentRegistry reads a JSON file mapping agent type names to AgentMeta.
func LoadAgentRegistry(path string) (*AgentRegistry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read agent registry: %w", err)
	}
	return ParseAgentRegistry(b)
}

// ParseAgentRegistry parses agent registry JSON bytes.
func ParseAgentRegistry(data []byte) (*AgentRegistry, error) {
	var raw map[string]*AgentMeta
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse agent registry: %w", err)
	}
	for name, meta := range raw {
		if meta.WasmName == "" {
			return nil, fmt.Errorf("agent %q: wasm_name is required", name)
		}
		if meta.Capability == "" {
			return nil, fmt.Errorf("agent %q: capability is required", name)
		}
		if meta.ContextKey == "" {
			return nil, fmt.Errorf("agent %q: context_key is required", name)
		}
	}
	return &AgentRegistry{agents: raw}, nil
}

// Get returns the metadata for an agent type, or an error if unknown.
func (r *AgentRegistry) Get(agentType string) (*AgentMeta, error) {
	meta, ok := r.agents[agentType]
	if !ok {
		return nil, fmt.Errorf("unknown agent type %q (not in agent registry)", agentType)
	}
	return meta, nil
}

// BuildPayload constructs the JSON payload string for a step, using the
// agent's payload_fields definition. Field values are resolved:
//   - "step.params.<key>" → step.Params[key]
//   - "task.context.<key>" → state.Context[key]
//   - numeric/bool literals → inlined as-is
func BuildPayload(meta *AgentMeta, state *taskstate.TaskState, step *taskstate.Step) string {
	if len(meta.PayloadFields) == 0 {
		return "{}"
	}

	out := make(map[string]any, len(meta.PayloadFields))
	for field, spec := range meta.PayloadFields {
		switch v := spec.(type) {
		case string:
			out[field] = resolveFieldRef(v, state, step)
		default:
			out[field] = v
		}
	}

	b, _ := json.Marshal(out)
	return string(b)
}

func resolveFieldRef(ref string, state *taskstate.TaskState, step *taskstate.Step) any {
	if key, ok := strings.CutPrefix(ref, "step.params."); ok {
		return step.Params[key]
	}
	if key, ok := strings.CutPrefix(ref, "task.context."); ok {
		return state.Context[key]
	}
	if n, err := strconv.ParseFloat(ref, 64); err == nil {
		if n == float64(int(n)) {
			return int(n)
		}
		return n
	}
	return ref
}
