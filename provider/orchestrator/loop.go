package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/jolucas/wasm-af/pkg/dag"
	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// spliceOutput is the generic format for a splice-flagged agent's output.
// The router agent produces {"skill": "shell", "params": {"command": "ls"}} —
// "skill" is accepted as an alias for "agent_type".
type spliceOutput struct {
	AgentType string            `json:"agent_type"`
	Skill     string            `json:"skill"`
	Params    map[string]string `json:"params"`
}

func (s *spliceOutput) resolvedAgentType() string {
	if s.AgentType != "" {
		return s.AgentType
	}
	return s.Skill
}

// buildDAG constructs a dag.Graph from the task's plan steps.
func buildDAG(plan []taskstate.Step) (*dag.Graph, error) {
	ids := make([]string, len(plan))
	deps := make(map[string][]string, len(plan))
	for i, s := range plan {
		ids[i] = s.ID
		if len(s.DependsOn) > 0 {
			deps[s.ID] = s.DependsOn
		}
	}
	return dag.New(ids, deps)
}

// stepIndex returns the index of a step ID in the plan, or -1 if not found.
func stepIndex(plan []taskstate.Step, id string) int {
	for i, s := range plan {
		if s.ID == id {
			return i
		}
	}
	return -1
}

// runTask is the main agent loop for a task. It runs in its own goroutine.
// Execution order is determined by the dependency DAG: steps whose dependencies
// are all satisfied run concurrently; splice-flagged steps may insert new steps
// into the plan at runtime.
func (o *Orchestrator) runTask(ctx context.Context, taskID string) {
	log := o.logger.With("task_id", taskID)

	if _, loaded := o.runningTasks.LoadOrStore(taskID, true); loaded {
		log.Info("runTask already active for this task, skipping")
		return
	}
	defer o.runningTasks.Delete(taskID)

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

	spliceCounter := 0

	iteration := 0
	for {
		iteration++
		iterStart := time.Now()

		g, err := buildDAG(state.Plan)
		if err != nil {
			log.Error("invalid plan DAG", "err", err)
			o.failTask(ctx, taskID, fmt.Sprintf("invalid plan DAG: %v", err))
			return
		}

		// The completed set determines which dependencies are satisfied.
		// Only actually-completed steps satisfy downstream dependencies.
		// Non-dispatchable steps (failed, denied, awaiting approval) are
		// filtered after Ready() to prevent re-dispatch without unblocking
		// their dependents.
		completed := make(map[string]bool)
		nonDispatchable := make(map[string]bool)
		for _, s := range state.Plan {
			switch s.Status {
			case taskstate.StepCompleted:
				completed[s.ID] = true
			case taskstate.StepFailed, taskstate.StepDenied, taskstate.StepAwaitingApproval:
				nonDispatchable[s.ID] = true
			}
		}

		readyIDs := g.Ready(completed)
		var dispatchable []string
		for _, id := range readyIDs {
			if !nonDispatchable[id] {
				dispatchable = append(dispatchable, id)
			}
		}
		if len(dispatchable) == 0 {
			log.Info("dag loop: no dispatchable steps, breaking",
				"iteration", iteration, "ready_total", len(readyIDs),
				"non_dispatchable", len(nonDispatchable))
			break
		}

		readyIndices := make([]int, 0, len(dispatchable))
		for _, id := range dispatchable {
			idx := stepIndex(state.Plan, id)
			if idx >= 0 {
				readyIndices = append(readyIndices, idx)
			}
		}

		log.Info("dag loop: dispatching batch",
			"iteration", iteration,
			"batch_size", len(dispatchable),
			"steps", dispatchable,
			"schedule_ms", time.Since(iterStart).Milliseconds())

		batchStart := time.Now()
		o.runParallelSteps(ctx, state, readyIndices)
		log.Info("dag loop: batch complete",
			"iteration", iteration,
			"batch_ms", time.Since(batchStart).Milliseconds())

		// Reload state after the batch completes (runStep persists results).
		state, err = o.store.Get(ctx, taskID)
		if err != nil {
			log.Error("failed to reload state", "err", err)
			o.failTask(ctx, taskID, "state reload failed")
			return
		}

		// Rebuild DAG from reloaded state for accurate child lookups.
		spliceG, spliceErr := buildDAG(state.Plan)
		if spliceErr != nil {
			log.Error("invalid plan DAG for splice", "err", spliceErr)
			o.failTask(ctx, taskID, fmt.Sprintf("invalid plan DAG: %v", spliceErr))
			return
		}

		// Handle splices for any splice-flagged steps that just completed.
		for _, id := range dispatchable {
			idx := stepIndex(state.Plan, id)
			if idx < 0 {
				continue
			}
			step := &state.Plan[idx]
			if step.Status != taskstate.StepCompleted {
				continue
			}
			meta, metaErr := o.registry.Get(step.AgentType)
			if metaErr != nil || !meta.Splice {
				continue
			}
			spliceCounter++
			if spliceErr := o.handleSplice(ctx, state, spliceG, step, spliceCounter); spliceErr != nil {
				log.Warn("splice failed, continuing without skill step", "step_id", step.ID, "err", spliceErr)
			}
		}

		// Reload state after potential splices.
		state, err = o.store.Get(ctx, taskID)
		if err != nil {
			log.Error("failed to reload state after splice", "err", err)
			o.failTask(ctx, taskID, "state reload failed")
			return
		}

		// If steps are awaiting approval and nothing else is ready, park
		// the task goroutine. The approval API will re-launch runTask.
		hasAwaitingApproval := false
		for _, s := range state.Plan {
			if s.Status == taskstate.StepAwaitingApproval {
				hasAwaitingApproval = true
				break
			}
		}
		if hasAwaitingApproval {
			nextG, _ := buildDAG(state.Plan)
			nextCompleted := make(map[string]bool)
			nextNonDispatch := make(map[string]bool)
			for _, s := range state.Plan {
				if s.Status == taskstate.StepCompleted {
					nextCompleted[s.ID] = true
				}
				if s.Status == taskstate.StepFailed || s.Status == taskstate.StepDenied || s.Status == taskstate.StepAwaitingApproval {
					nextNonDispatch[s.ID] = true
				}
			}
			nextReady := nextG.Ready(nextCompleted)
			canDispatch := false
			for _, id := range nextReady {
				if !nextNonDispatch[id] {
					canDispatch = true
					break
				}
			}
			if nextG == nil || !canDispatch {
				log.Info("task parked, awaiting approval")
				return
			}
		}
	}

	// Determine terminal state. A step is "live" if it's pending or
	// awaiting approval AND none of its transitive ancestors are
	// failed/denied (which would make it a dead branch).
	finalG, _ := buildDAG(state.Plan)
	terminalStatuses := map[string]bool{}
	for _, s := range state.Plan {
		if s.Status == taskstate.StepFailed || s.Status == taskstate.StepDenied {
			terminalStatuses[s.ID] = true
		}
	}

	hasLivePending := false
	hasAwaiting := false
	for _, s := range state.Plan {
		if s.Status == taskstate.StepAwaitingApproval {
			hasAwaiting = true
			continue
		}
		if s.Status != taskstate.StepPending {
			continue
		}
		// A pending step whose ancestors include a failed/denied step
		// is a dead branch — it will never run. Don't count it.
		dead := false
		if finalG != nil {
			for _, aid := range finalG.Ancestors(s.ID) {
				if terminalStatuses[aid] {
					dead = true
					break
				}
			}
		}
		if !dead {
			hasLivePending = true
		}
	}

	if hasAwaiting || hasLivePending {
		if hasAwaiting {
			log.Info("task parked, awaiting approval")
		} else {
			o.failTask(ctx, taskID, "task stuck: pending steps with unmet dependencies")
		}
		return
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
// Individual step failures are persisted by runStep (status + audit) and
// logged here but do not kill the task — other branches continue.
func (o *Orchestrator) runParallelSteps(ctx context.Context, state *taskstate.TaskState, indices []int) {
	var wg sync.WaitGroup

	for _, idx := range indices {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := o.runStep(ctx, state, idx); err != nil {
				o.logger.Warn("step failed", "step_id", state.Plan[idx].ID, "err", err)
			}
		}(idx)
	}

	wg.Wait()
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
		idx := stepIndex(s.Plan, step.ID)
		if idx < 0 {
			return fmt.Errorf("step %s not found in plan", step.ID)
		}
		s.Plan[idx].Status = taskstate.StepRunning
		s.Plan[idx].StartedAt = &now
		s.CurrentStep = idx
		return nil
	}); err != nil {
		return fmt.Errorf("mark step running: %w", err)
	}

	if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventStepStarted,
	}); err != nil {
		log.Error("audit write failed", "event", taskstate.EventStepStarted, "err", err)
	}

	meta, err := o.registry.Get(step.AgentType)
	if err != nil {
		return fmt.Errorf("agent registry: %w", err)
	}

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
			idx := stepIndex(s.Plan, step.ID)
			if idx < 0 {
				return nil
			}
			s.Plan[idx].Status = taskstate.StepDenied
			s.Plan[idx].Error = denyMsg
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
		log.Info("step denied")
		return fmt.Errorf("policy denied: %s", denyMsg)
	}

	if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventPolicyPermit,
		PolicySource: policySource, PolicyCapability: meta.Capability,
	}); err != nil {
		log.Error("audit write failed", "event", taskstate.EventPolicyPermit, "err", err)
	}

	if result.RequiresApproval && step.ApprovedBy == "" {
		reason := "policy requires approval"
		if result.ApprovalReason != "" {
			reason = result.ApprovalReason
		}
		if err := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
			idx := stepIndex(s.Plan, step.ID)
			if idx < 0 {
				return fmt.Errorf("step %s not found", step.ID)
			}
			s.Plan[idx].Status = taskstate.StepAwaitingApproval
			s.Plan[idx].ApprovalReason = reason
			return nil
		}); err != nil {
			return fmt.Errorf("mark step awaiting approval: %w", err)
		}
		if auditErr := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID: taskID, StepID: step.ID,
			EventType: taskstate.EventStepAwaitingApproval,
			Message:   reason,
		}); auditErr != nil {
			log.Error("audit write failed", "event", taskstate.EventStepAwaitingApproval, "err", auditErr)
		}
		o.publishApprovalNeeded(ctx, taskID, step.ID, step.AgentType, reason)
		log.Info("step awaiting approval", "reason", reason)
		return nil
	}

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

	hostFnNames := meta.HostFunctions
	if len(result.HostFunctions) > 0 {
		hostFnNames = result.HostFunctions
	}
	opts.HostFunctions = o.hostFns.Resolve(hostFnNames, o)

	inputPayload := BuildPayload(meta, state, step)
	inputContext := o.buildStepContext(state, step.ID)

	input := &TaskInput{
		TaskID:  taskID,
		StepID:  step.ID,
		Payload: inputPayload,
		Context: taskInputContext(inputContext),
	}

	if err := o.store.PutPayload(ctx, step.InputKey, inputPayload); err != nil {
		return fmt.Errorf("write input payload: %w", err)
	}

	ctx = withStepMeta(ctx, StepMeta{
		TaskID:    taskID,
		StepID:    step.ID,
		AgentType: step.AgentType,
	})

	output, err := o.invokeAgent(ctx, meta.WasmName, input, opts)
	if err != nil {
		if updateErr := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
			idx := stepIndex(s.Plan, step.ID)
			if idx < 0 {
				return nil
			}
			s.Plan[idx].Status = taskstate.StepFailed
			s.Plan[idx].Error = err.Error()
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
		idx := stepIndex(s.Plan, step.ID)
		if idx < 0 {
			return nil
		}
		s.Plan[idx].Status = taskstate.StepCompleted
		s.Plan[idx].CompletedAt = &fin
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

// buildStepContext assembles context from ancestor step outputs (following the
// dependency DAG). When multiple ancestors share the same context key (parallel
// fan-out), their results are merged into a single JSON array.
func (o *Orchestrator) buildStepContext(state *taskstate.TaskState, stepID string) []contextPair {
	g, err := buildDAG(state.Plan)
	if err != nil {
		return nil
	}

	ancestorIDs := g.Ancestors(stepID)
	ancestorSet := make(map[string]bool, len(ancestorIDs))
	for _, id := range ancestorIDs {
		ancestorSet[id] = true
	}

	type entry struct {
		key    string
		values []string
	}

	seen := make(map[string]int)
	var entries []entry

	for _, s := range state.Plan {
		if !ancestorSet[s.ID] {
			continue
		}
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

// mergeOutputs wraps multiple step outputs into a JSON array.
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

// handleSplice reads a splice-flagged step's output, validates the proposed
// agent type against OPA, and inserts a new step into the plan. Steps that
// depended on the splicing step are rewired to depend on the new step.
func (o *Orchestrator) handleSplice(ctx context.Context, state *taskstate.TaskState, g *dag.Graph, step *taskstate.Step, counter int) error {
	outputJSON, ok := state.Results[step.OutputKey]
	if !ok {
		return fmt.Errorf("splice step output not found in results")
	}

	var splice spliceOutput
	if err := json.Unmarshal([]byte(outputJSON), &splice); err != nil {
		return fmt.Errorf("parse splice output: %w", err)
	}

	agentType := splice.resolvedAgentType()
	o.logger.Info("splice decision", "agent_type", agentType, "task_id", state.TaskID, "source_step", step.ID)

	if agentType == "" || agentType == "direct-answer" {
		return nil
	}

	taskID := state.TaskID

	if o.policy == nil {
		_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID:        taskID,
			EventType:     taskstate.EventPolicyDeny,
			PolicySource:  "wasm-af:router-splice",
			PolicyDenyMsg: "no policy loaded; deny-all",
			Message:       fmt.Sprintf("proposed agent %q denied: no policy loaded", agentType),
		})
		o.logger.Warn("splice denied: no policy loaded", "agent_type", agentType)
		return nil
	}

	policyInput := map[string]any{
		"step": map[string]any{
			"agent_type": "router-splice",
			"params": map[string]any{
				"proposed_skill": agentType,
			},
		},
		"task": map[string]any{
			"id":   taskID,
			"type": state.Context["type"],
		},
	}
	result, err := o.policy.EvaluateStep(ctx, policyInput)
	if err != nil {
		return fmt.Errorf("splice policy: %w", err)
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
			Message:       fmt.Sprintf("proposed agent %q denied by policy", agentType),
		})
		o.logger.Warn("splice denied by policy", "agent_type", agentType, "deny", denyMsg)
		return nil
	}

	newStepID := fmt.Sprintf("%s-splice-%d", taskID, counter)
	dependents := g.Children(step.ID)

	newStep := taskstate.Step{
		ID:        newStepID,
		AgentType: agentType,
		InputKey:  newStepID + ".input",
		OutputKey: newStepID + ".output",
		Status:    taskstate.StepPending,
		DependsOn: []string{step.ID},
		Params:    splice.Params,
	}

	return o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		// Append the new step to the plan.
		s.Plan = append(s.Plan, newStep)

		// Rewire: steps that depended on the splicing step now depend on
		// the new step instead.
		for i := range s.Plan {
			for _, depID := range dependents {
				if s.Plan[i].ID == depID {
					s.Plan[i].DependsOn = replaceDep(s.Plan[i].DependsOn, step.ID, newStepID)
				}
			}
		}
		return nil
	})
}

// replaceDep replaces one dependency ID with another in a DependsOn list.
func replaceDep(deps []string, old, new string) []string {
	out := make([]string, len(deps))
	copy(out, deps)
	for i, d := range out {
		if d == old {
			out[i] = new
			return out
		}
	}
	return out
}
