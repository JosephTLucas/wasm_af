package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// routerOutput mirrors the JSON returned by the router WASM agent.
type routerOutput struct {
	Skill  string       `json:"skill"`
	Params routerParams `json:"params"`
}

type routerParams struct {
	Query    string `json:"query"`
	Command  string `json:"command"`
	Path     string `json:"path"`
	Content  string `json:"content"`
	Op       string `json:"op"`
	Code     string `json:"code"`
	Language string `json:"language"`
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
			stepAgentType := state.Plan[i].AgentType
			if err := o.runStep(ctx, state, i); err != nil {
				log.Error("step failed", "step_id", state.Plan[i].ID, "err", err)
				o.failTask(ctx, taskID, fmt.Sprintf("step %s failed: %v", state.Plan[i].ID, err))
				return
			}
			// After a router step completes, reload state (runStep stored the
			// output) then validate and splice the skill step.
			if stepAgentType == "router" {
				state, err = o.store.Get(ctx, taskID)
				if err != nil {
					log.Error("failed to reload state for splice", "err", err)
					o.failTask(ctx, taskID, "state reload failed")
					return
				}
				if err := o.spliceRoutedStep(ctx, state, i); err != nil {
					log.Warn("routing splice failed, continuing without skill step", "err", err)
				}
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

	if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID:    taskID,
		EventType: taskstate.EventTaskCompleted,
	}); err != nil {
		log.Error("audit write failed", "event", taskstate.EventTaskCompleted, "err", err)
	}
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

	if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventStepStarted,
	}); err != nil {
		log.Error("audit write failed", "event", taskstate.EventStepStarted, "err", err)
	}

	// ── AGENT METADATA ───────────────────────────────────────────────────────
	meta, err := o.registry.Get(step.AgentType)
	if err != nil {
		return fmt.Errorf("agent registry: %w", err)
	}

	// ── POLICY EVALUATION ────────────────────────────────────────────────────
	policySource := "wasm-af:" + step.AgentType

	result, err := o.evaluateStepPolicy(ctx, state, step, meta, stepIdx)
	if err != nil {
		return fmt.Errorf("policy evaluation failed: %w", err)
	}

	if !result.Permitted {
		denyMsg := "denied"
		if result.DenyMessage != nil {
			denyMsg = *result.DenyMessage
		}
		if err := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
			s.Plan[stepIdx].Status = taskstate.StepDenied
			s.Plan[stepIdx].Error = denyMsg
			return nil
		}); err != nil {
			log.Error("failed to mark step denied", "err", err)
		}
		if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID: taskID, StepID: step.ID, EventType: taskstate.EventPolicyDeny,
			PolicySource: policySource, PolicyCapability: meta.Capability,
			PolicyDenyMsg: denyMsg,
		}); err != nil {
			log.Error("audit write failed", "event", taskstate.EventPolicyDeny, "err", err)
		}
		return fmt.Errorf("policy denied: %s", denyMsg)
	}

	if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventPolicyPermit,
		PolicySource: policySource, PolicyCapability: meta.Capability,
	}); err != nil {
		log.Error("audit write failed", "event", taskstate.EventPolicyPermit, "err", err)
	}

	// ── BUILD PLUGIN OPTS (defaults + policy overrides) ─────────────────────
	opts := PluginOpts{
		MaxMemPages:  o.pluginMaxMemoryPages,
		MaxHTTPBytes: o.pluginMaxHTTPBytes,
		Timeout:      o.pluginTimeout,
	}

	if len(result.AllowedHosts) > 0 {
		opts.AllowedHosts = result.AllowedHosts
	}
	if result.MaxMemPages != nil {
		opts.MaxMemPages = *result.MaxMemPages
	}
	if result.MaxHTTPBytes != nil {
		opts.MaxHTTPBytes = *result.MaxHTTPBytes
	}
	if result.TimeoutSec != nil {
		opts.Timeout = time.Duration(*result.TimeoutSec) * time.Second
	}
	if len(result.Config) > 0 {
		opts.Config = result.Config
	}
	if len(result.AllowedPaths) > 0 {
		opts.AllowedPaths = result.AllowedPaths
	}

	// Resolve host functions: policy can override which ones are injected.
	hostFnNames := meta.HostFunctions
	if len(result.HostFunctions) > 0 {
		hostFnNames = result.HostFunctions
	}
	opts.HostFunctions = o.hostFns.Resolve(hostFnNames, o)

	// ── INVOKE AGENT ─────────────────────────────────────────────────────────
	inputPayload := BuildPayload(meta, state, step)
	inputContext := o.buildStepContext(state, stepIdx)

	input := &TaskInput{
		TaskID:  taskID,
		StepID:  step.ID,
		Payload: inputPayload,
		Context: taskInputContext(inputContext),
	}

	if err := o.store.PutPayload(ctx, step.InputKey, inputPayload); err != nil {
		return fmt.Errorf("write input payload: %w", err)
	}

	output, err := o.invokeAgent(ctx, meta.WasmName, input, opts)
	if err != nil {
		if updateErr := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
			s.Plan[stepIdx].Status = taskstate.StepFailed
			s.Plan[stepIdx].Error = err.Error()
			return nil
		}); updateErr != nil {
			log.Error("failed to mark step failed", "err", updateErr)
		}
		if auditErr := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID: taskID, StepID: step.ID, EventType: taskstate.EventStepFailed,
			Message: err.Error(),
		}); auditErr != nil {
			log.Error("audit write failed", "event", taskstate.EventStepFailed, "err", auditErr)
		}
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

	if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventStepCompleted,
	}); err != nil {
		log.Error("audit write failed", "event", taskstate.EventStepCompleted, "err", err)
	}
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
	if err := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		s.Status = taskstate.StatusFailed
		s.Error = reason
		return nil
	}); err != nil {
		o.logger.Error("failed to mark task failed", "task_id", taskID, "err", err)
	}
	if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, EventType: taskstate.EventTaskFailed, Message: reason,
	}); err != nil {
		o.logger.Error("audit write failed", "task_id", taskID, "event", taskstate.EventTaskFailed, "err", err)
	}
}

// buildStepContext assembles context from prior step outputs.
// When multiple prior steps share the same context key (parallel fan-out),
// their results are merged into a single JSON value.
func (o *Orchestrator) buildStepContext(state *taskstate.TaskState, stepIdx int) []contextPair {
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
		key := o.contextKeyForAgent(s.AgentType)
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
			ctx = append(ctx, contextPair{Key: e.key, Val: mergeOutputs(e.values)})
		}
	}
	return ctx
}

// mergeOutputs wraps multiple step outputs into a JSON array. The
// orchestrator is format-agnostic; the consuming agent decides how
// to interpret the array elements.
func mergeOutputs(outputs []string) string {
	merged := make([]json.RawMessage, 0, len(outputs))
	for _, raw := range outputs {
		merged = append(merged, json.RawMessage(raw))
	}
	b, _ := json.Marshal(merged)
	return string(b)
}

func (o *Orchestrator) contextKeyForAgent(agentType string) string {
	meta, err := o.registry.Get(agentType)
	if err != nil {
		return agentType + "_result"
	}
	return meta.ContextKey
}

// spliceRoutedStep reads the router output, validates the proposed skill
// against OPA, and splices a concrete skill step into the plan immediately
// after the router step. If the skill is "direct-answer" or policy denies it,
// no step is added and the responder receives only prior context.
func (o *Orchestrator) spliceRoutedStep(ctx context.Context, state *taskstate.TaskState, routerIdx int) error {
	routerStep := state.Plan[routerIdx]
	routerJSON, ok := state.Results[routerStep.OutputKey]
	if !ok {
		return fmt.Errorf("router output not found in results")
	}

	var route routerOutput
	if err := json.Unmarshal([]byte(routerJSON), &route); err != nil {
		return fmt.Errorf("parse router output: %w", err)
	}

	// direct-answer means no skill step needed.
	if route.Skill == "" || route.Skill == "direct-answer" {
		return nil
	}

	taskID := state.TaskID

	// Policy gate: evaluate the proposed skill splice (fail closed).
	if o.policy == nil {
		_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID:        taskID,
			EventType:     taskstate.EventPolicyDeny,
			PolicySource:  "wasm-af:router-splice",
			PolicyDenyMsg: "no policy loaded; deny-all",
			Message:       fmt.Sprintf("proposed skill %q denied: no policy loaded", route.Skill),
		})
		o.logger.Warn("router-splice denied: no policy loaded", "skill", route.Skill)
		return nil
	}

	policyInput := map[string]any{
		"step": map[string]any{
			"agent_type": "router-splice",
			"params": map[string]any{
				"proposed_skill": route.Skill,
			},
		},
		"task": map[string]any{
			"id":   taskID,
			"type": state.Context["type"],
		},
	}
	result, err := o.policy.EvaluateStep(ctx, policyInput)
	if err != nil {
		return fmt.Errorf("router-splice policy: %w", err)
	}
	if !result.Permitted {
		denyMsg := "denied"
		if result.DenyMessage != nil {
			denyMsg = *result.DenyMessage
		}
		_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID:        taskID,
			EventType:     taskstate.EventPolicyDeny,
			PolicySource:  "wasm-af:router-splice",
			PolicyDenyMsg: denyMsg,
			Message:       fmt.Sprintf("proposed skill %q denied by policy", route.Skill),
		})
		o.logger.Warn("router-splice denied by policy", "skill", route.Skill, "deny", denyMsg)
		return nil // not a task error — just skip the skill
	}

	// Build step params from the router's extracted fields.
	params := make(map[string]string)
	switch route.Skill {
	case "web-search":
		if route.Params.Query != "" {
			params["query"] = route.Params.Query
		}
	case "shell":
		if route.Params.Command != "" {
			params["command"] = route.Params.Command
		}
	case "file-ops":
		if route.Params.Op != "" {
			params["op"] = route.Params.Op
		}
		if route.Params.Path != "" {
			params["path"] = route.Params.Path
		}
		if route.Params.Content != "" {
			params["content"] = route.Params.Content
		}
	case "sandbox-exec":
		if route.Params.Code != "" {
			params["code"] = route.Params.Code
		}
		if route.Params.Language != "" {
			params["language"] = route.Params.Language
		}
	}

	skillStepID := fmt.Sprintf("%s-skill", taskID)
	newStep := taskstate.Step{
		ID:        skillStepID,
		AgentType: route.Skill,
		InputKey:  skillStepID + ".input",
		OutputKey: skillStepID + ".output",
		Status:    taskstate.StepPending,
		Params:    params,
	}

	// Splice into the plan via CAS update.
	return o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		insertAt := routerIdx + 1
		newPlan := make([]taskstate.Step, 0, len(s.Plan)+1)
		newPlan = append(newPlan, s.Plan[:insertAt]...)
		newPlan = append(newPlan, newStep)
		newPlan = append(newPlan, s.Plan[insertAt:]...)
		s.Plan = newPlan
		return nil
	})
}
