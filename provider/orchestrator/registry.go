package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

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
	Splice        bool              `json:"splice,omitempty"`
	External      bool              `json:"external,omitempty"`
}

// AgentRegistry holds the set of known agent types. It is safe for
// concurrent use — platform agents are loaded at startup from JSON and
// external (BYOA) agents can be registered/removed at runtime.
type AgentRegistry struct {
	mu     sync.RWMutex
	agents map[string]*AgentMeta
}

// ParseAgentRegistry parses agent registry JSON bytes.
func ParseAgentRegistry(data []byte) (*AgentRegistry, error) {
	var raw map[string]*AgentMeta
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse agent registry: %w", err)
	}
	for name, meta := range raw {
		if err := validateAgentMeta(name, meta); err != nil {
			return nil, err
		}
	}
	return &AgentRegistry{agents: raw}, nil
}

func validateAgentMeta(name string, meta *AgentMeta) error {
	if meta.WasmName == "" {
		return fmt.Errorf("agent %q: wasm_name is required", name)
	}
	if meta.Capability == "" {
		return fmt.Errorf("agent %q: capability is required", name)
	}
	if meta.ContextKey == "" {
		return fmt.Errorf("agent %q: context_key is required", name)
	}
	return nil
}

// Get returns the metadata for an agent type, or an error if unknown.
func (r *AgentRegistry) Get(agentType string) (*AgentMeta, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	meta, ok := r.agents[agentType]
	if !ok {
		return nil, fmt.Errorf("unknown agent type %q (not in agent registry)", agentType)
	}
	return meta, nil
}

// Register adds or replaces an agent in the registry. The same
// validation rules that apply at startup are enforced here.
func (r *AgentRegistry) Register(name string, meta *AgentMeta) error {
	if err := validateAgentMeta(name, meta); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.agents[name] = meta
	return nil
}

// Remove deletes an agent from the registry. It is a no-op if the
// agent does not exist.
func (r *AgentRegistry) Remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.agents, name)
}

// List returns a snapshot of all registered agents keyed by name.
func (r *AgentRegistry) List() map[string]*AgentMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*AgentMeta, len(r.agents))
	for k, v := range r.agents {
		out[k] = v
	}
	return out
}

// ListExternal returns a snapshot of only the external (BYOA) agents.
func (r *AgentRegistry) ListExternal() map[string]*AgentMeta {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*AgentMeta)
	for k, v := range r.agents {
		if v.External {
			out[k] = v
		}
	}
	return out
}

// ClearExternal removes all agents with External == true.
func (r *AgentRegistry) ClearExternal() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.agents {
		if v.External {
			delete(r.agents, k)
		}
	}
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
