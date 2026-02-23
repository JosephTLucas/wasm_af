package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	extism "github.com/extism/go-sdk"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// wasmNameForAgent maps an agent type to the compiled .wasm filename (without extension).
func wasmNameForAgent(agentType string) string {
	return strings.ReplaceAll(agentType, "-", "_")
}

// runTask is the main agent loop for a task. It runs in its own goroutine.
func (o *Orchestrator) runTask(ctx context.Context, taskID string) {
	log := o.logger.With("task_id", taskID)

	if err := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		s.Status = taskstate.StatusRunning
		return nil
	}); err != nil {
		log.Error("failed to mark task running", "err", err)
		return
	}

	state, err := o.store.Get(ctx, taskID)
	if err != nil {
		log.Error("failed to load task state", "err", err)
		o.failTask(ctx, taskID, "failed to load task state")
		return
	}

	i := 0
	for i < len(state.Plan) {
		if state.Plan[i].Status != taskstate.StepPending {
			i++
			continue
		}

		if group := state.Plan[i].Group; group != "" {
			var indices []int
			for j := i; j < len(state.Plan) && state.Plan[j].Group == group; j++ {
				if state.Plan[j].Status == taskstate.StepPending {
					indices = append(indices, j)
				}
			}

			if err := o.runParallelSteps(ctx, state, indices); err != nil {
				log.Error("parallel group failed", "group", group, "err", err)
				o.failTask(ctx, taskID, fmt.Sprintf("parallel group %q failed: %v", group, err))
				return
			}

			for i < len(state.Plan) && state.Plan[i].Group == group {
				i++
			}
		} else {
			if err := o.runStep(ctx, state, i); err != nil {
				log.Error("step failed", "step_id", state.Plan[i].ID, "err", err)
				o.failTask(ctx, taskID, fmt.Sprintf("step %s failed: %v", state.Plan[i].ID, err))
				return
			}
			i++
		}

		state, err = o.store.Get(ctx, taskID)
		if err != nil {
			log.Error("failed to reload state", "err", err)
			o.failTask(ctx, taskID, "state reload failed")
			return
		}
	}

	if err := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		s.Status = taskstate.StatusCompleted
		return nil
	}); err != nil {
		log.Error("failed to mark task completed", "err", err)
	}

	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID:    taskID,
		EventType: taskstate.EventTaskCompleted,
	})
	log.Info("task completed")
}

// runParallelSteps executes a batch of plan steps concurrently.
func (o *Orchestrator) runParallelSteps(ctx context.Context, state *taskstate.TaskState, indices []int) error {
	errs := make([]error, len(indices))
	var wg sync.WaitGroup

	for slot, idx := range indices {
		wg.Add(1)
		go func(slot, idx int) {
			defer wg.Done()
			errs[slot] = o.runStep(ctx, state, idx)
		}(slot, idx)
	}

	wg.Wait()
	return errors.Join(errs...)
}

// runStep executes a single plan step:
//  1. Evaluate policy.
//  2. Create an Extism plugin with scoped capabilities.
//  3. Call the agent's "execute" export.
//  4. Destroy the plugin, store the output.
func (o *Orchestrator) runStep(ctx context.Context, state *taskstate.TaskState, stepIdx int) error {
	step := &state.Plan[stepIdx]
	taskID := state.TaskID
	log := o.logger.With("task_id", taskID, "step_id", step.ID, "agent_type", step.AgentType)
	log.Info("starting step")

	now := time.Now().UTC()
	if err := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		s.Plan[stepIdx].Status = taskstate.StepRunning
		s.Plan[stepIdx].StartedAt = &now
		s.CurrentStep = stepIdx
		return nil
	}); err != nil {
		return fmt.Errorf("mark step running: %w", err)
	}

	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventStepStarted,
	})

	// ── POLICY EVALUATION ────────────────────────────────────────────────────
	policySource := "wasm-af:" + step.AgentType
	cap := capabilityForAgent(step.AgentType)

	result, err := o.evaluatePolicy(ctx, taskID, step.ID, policySource, "*", string(cap))
	if err != nil {
		return fmt.Errorf("policy evaluation failed: %w", err)
	}

	if !result.Permitted {
		denyMsg := "denied"
		if result.DenyMessage != nil {
			denyMsg = *result.DenyMessage
		}
		_ = o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
			s.Plan[stepIdx].Status = taskstate.StepDenied
			s.Plan[stepIdx].Error = denyMsg
			return nil
		})
		_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID: taskID, StepID: step.ID, EventType: taskstate.EventPolicyDeny,
			PolicySource: policySource, PolicyCapability: string(cap),
			PolicyDenyMsg: denyMsg,
		})
		return fmt.Errorf("policy denied: %s", denyMsg)
	}

	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventPolicyPermit,
		PolicySource: policySource, PolicyCapability: string(cap),
	})

	// ── BUILD ALLOWED HOSTS ──────────────────────────────────────────────────
	var allowedHosts []string
	if step.AllowedHosts != "" {
		allowedHosts = []string{step.AllowedHosts}
	}

	// ── BUILD HOST FUNCTIONS ─────────────────────────────────────────────────
	var hostFunctions []extism.HostFunction
	if step.AgentType == "summarizer" {
		hostFunctions = o.llmHostFunctions()
	}

	// ── INVOKE AGENT ─────────────────────────────────────────────────────────
	inputPayload := buildStepPayload(state, stepIdx)
	inputContext := buildStepContext(state, stepIdx)

	input := &TaskInput{
		TaskID:  taskID,
		StepID:  step.ID,
		Payload: inputPayload,
		Context: taskInputContext(inputContext),
	}

	if err := o.store.PutPayload(ctx, step.InputKey, inputPayload); err != nil {
		return fmt.Errorf("write input payload: %w", err)
	}

	wasmName := wasmNameForAgent(step.AgentType)
	output, err := o.invokeAgent(ctx, wasmName, input, allowedHosts, hostFunctions)
	if err != nil {
		_ = o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
			s.Plan[stepIdx].Status = taskstate.StepFailed
			s.Plan[stepIdx].Error = err.Error()
			return nil
		})
		_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID: taskID, StepID: step.ID, EventType: taskstate.EventStepFailed,
			Message: err.Error(),
		})
		return fmt.Errorf("agent invocation: %w", err)
	}

	if err := o.store.PutPayload(ctx, step.OutputKey, output.Payload); err != nil {
		return fmt.Errorf("write output payload: %w", err)
	}

	fin := time.Now().UTC()
	if err := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		s.Plan[stepIdx].Status = taskstate.StepCompleted
		s.Plan[stepIdx].CompletedAt = &fin
		s.Results[step.OutputKey] = output.Payload
		return nil
	}); err != nil {
		return fmt.Errorf("mark step completed: %w", err)
	}

	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventStepCompleted,
	})
	log.Info("step completed")
	return nil
}

func taskInputContext(pairs []contextPair) []KVPair {
	out := make([]KVPair, len(pairs))
	for i, p := range pairs {
		out[i] = KVPair{Key: p.Key, Val: p.Val}
	}
	return out
}

type contextPair struct {
	Key string
	Val string
}

// failTask marks a task as failed and writes a terminal audit event.
func (o *Orchestrator) failTask(ctx context.Context, taskID, reason string) {
	_ = o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		s.Status = taskstate.StatusFailed
		s.Error = reason
		return nil
	})
	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, EventType: taskstate.EventTaskFailed, Message: reason,
	})
}

// buildStepPayload returns the JSON payload string for a specific step.
func buildStepPayload(state *taskstate.TaskState, stepIdx int) string {
	step := &state.Plan[stepIdx]
	switch step.AgentType {
	case "web-search":
		type wsPayload struct {
			Query string `json:"query"`
			Count int    `json:"count"`
		}
		b, _ := json.Marshal(wsPayload{Query: state.Context["query"], Count: 5})
		return string(b)
	case "url-fetch":
		type fetchPayload struct {
			URL string `json:"url"`
		}
		b, _ := json.Marshal(fetchPayload{URL: step.Params["url"]})
		return string(b)
	case "summarizer":
		type sumPayload struct {
			Query string `json:"query,omitempty"`
		}
		b, _ := json.Marshal(sumPayload{Query: state.Context["query"]})
		return string(b)
	default:
		type generic struct {
			Query   string            `json:"query,omitempty"`
			Context map[string]string `json:"context,omitempty"`
		}
		b, _ := json.Marshal(generic{Query: state.Context["query"], Context: state.Context})
		return string(b)
	}
}

// buildStepContext assembles context from prior step outputs.
// When multiple prior steps share the same context key (parallel fan-out),
// their results are merged into a single JSON value.
func buildStepContext(state *taskstate.TaskState, stepIdx int) []contextPair {
	type entry struct {
		key    string
		values []string
	}

	seen := make(map[string]int)
	var entries []entry

	for i := 0; i < stepIdx; i++ {
		s := state.Plan[i]
		v, ok := state.Results[s.OutputKey]
		if !ok {
			continue
		}
		key := contextKeyForAgent(s.AgentType)
		if idx, exists := seen[key]; exists {
			entries[idx].values = append(entries[idx].values, v)
		} else {
			seen[key] = len(entries)
			entries = append(entries, entry{key: key, values: []string{v}})
		}
	}

	var ctx []contextPair
	for _, e := range entries {
		if len(e.values) == 1 {
			ctx = append(ctx, contextPair{Key: e.key, Val: e.values[0]})
		} else {
			ctx = append(ctx, contextPair{Key: e.key, Val: mergeSearchOutputs(e.values)})
		}
	}
	return ctx
}

func mergeSearchOutputs(outputs []string) string {
	type searchOutput struct {
		Query   string            `json:"query"`
		Results []json.RawMessage `json:"results"`
	}

	var merged searchOutput
	var queries []string
	for _, raw := range outputs {
		var so searchOutput
		if err := json.Unmarshal([]byte(raw), &so); err == nil {
			queries = append(queries, so.Query)
			merged.Results = append(merged.Results, so.Results...)
		}
	}
	merged.Query = strings.Join(queries, " | ")
	b, _ := json.Marshal(merged)
	return string(b)
}

func contextKeyForAgent(agentType string) string {
	switch agentType {
	case "web-search", "url-fetch":
		return "web_search_results"
	case "summarizer":
		return "summary_result"
	default:
		return agentType + "_result"
	}
}

// PolicyCapability mirrors the policy engine's capability enum.
type PolicyCapability string

const (
	CapHTTP PolicyCapability = "http"
	CapLLM  PolicyCapability = "llm"
)

func capabilityForAgent(agentType string) PolicyCapability {
	switch agentType {
	case "web-search", "url-fetch":
		return CapHTTP
	case "summarizer":
		return CapLLM
	default:
		return CapHTTP
	}
}
