package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// agentImageRef returns the OCI image reference for a given agent type.
// Falls back to a local registry convention if no explicit ref is configured.
func (o *Orchestrator) agentImageRef(agentType string) string {
	if ref, ok := o.agentRefs[agentType]; ok {
		return ref
	}
	return fmt.Sprintf("localhost:5000/agent-%s:latest", agentType)
}

// runTask is the main agent loop for a task. It runs in its own goroutine.
// Steps are executed in sequence; on any error the task is marked failed.
func (o *Orchestrator) runTask(ctx context.Context, taskID string) {
	ctx, span := tracer.Start(ctx, "orchestrator.run_task")
	defer span.End()
	span.SetAttributes(attribute.String("task.id", taskID))

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

	for i := range state.Plan {
		if state.Plan[i].Status != taskstate.StepPending {
			continue
		}
		if err := o.runStep(ctx, state, i); err != nil {
			log.Error("step failed", "step_id", state.Plan[i].ID, "err", err)
			o.failTask(ctx, taskID, fmt.Sprintf("step %s failed: %v", state.Plan[i].ID, err))
			return
		}
		// Re-read state so the next step sees accumulated results.
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
		span.RecordError(err)
	}

	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID:    taskID,
		EventType: taskstate.EventTaskCompleted,
	})
	span.SetStatus(codes.Ok, "task completed")
	log.Info("task completed")
}

// runStep executes a single plan step. It:
//  1. Evaluates policy for the primary capability link.
//  2. Starts the agent component on an available host.
//  3. Creates the capability link (and optionally a direct peer link).
//  4. Invokes the agent via wRPC.
//  5. Stores the output and tears down links and component.
func (o *Orchestrator) runStep(ctx context.Context, state *taskstate.TaskState, stepIdx int) error {
	step := &state.Plan[stepIdx]
	taskID := state.TaskID

	ctx, span := tracer.Start(ctx, "orchestrator.run_step")
	defer span.End()
	span.SetAttributes(
		attribute.String("task.id", taskID),
		attribute.String("step.id", step.ID),
		attribute.String("step.agent_type", step.AgentType),
	)

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

	componentID := fmt.Sprintf("%s-%s", taskID, step.AgentType)
	imageRef := o.agentImageRef(step.AgentType)
	cap := capabilityForAgent(step.AgentType)

	// Hosts — pick first available.
	hosts, err := o.ctl.GetHosts(ctx)
	if err != nil || len(hosts) == 0 {
		return fmt.Errorf("no hosts available: %w", err)
	}
	hostID := hosts[0].ID

	// ── POLICY EVALUATION ────────────────────────────────────────────────────
	// No link is created without a permit. Every decision is audited.
	mode, err := o.EvaluateLink(ctx, taskID, step.ID, componentID, o.providerIDForCap(cap), cap)
	if err != nil {
		_ = o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
			s.Plan[stepIdx].Status = taskstate.StepDenied
			s.Plan[stepIdx].Error = err.Error()
			return nil
		})
		_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
			TaskID: taskID, StepID: step.ID, EventType: taskstate.EventStepFailed,
			Message: err.Error(),
		})
		return fmt.Errorf("policy denied: %w", err)
	}
	span.SetAttributes(attribute.String("policy.comms_mode", string(mode)))

	// ── COMPONENT START ───────────────────────────────────────────────────────
	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventComponentStart,
		ComponentID: componentID, ComponentRef: imageRef,
	})
	if err := o.ctl.StartComponent(ctx, hostID, imageRef, componentID); err != nil {
		return fmt.Errorf("start component %s: %w", componentID, err)
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := o.ctl.StopComponent(stopCtx, hostID, componentID); err != nil {
			log.Warn("stop component failed", "component_id", componentID, "err", err)
		}
		_ = o.store.AppendAudit(stopCtx, &taskstate.AuditEvent{
			TaskID: taskID, StepID: step.ID, EventType: taskstate.EventComponentStop,
			ComponentID: componentID,
		})
	}()

	// ── CAPABILITY LINK ───────────────────────────────────────────────────────
	capLink := o.linkForStep(step.AgentType, componentID, o.providerIDForCap(cap), cap)
	if err := o.ctl.PutLink(ctx, capLink); err != nil {
		return fmt.Errorf("put capability link: %w", err)
	}
	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventLinkCreated,
		ComponentID: componentID, ComponentRef: o.providerIDForCap(cap),
	})
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = o.ctl.DeleteLink(delCtx, componentID, "default", capLink.WitNamespace, capLink.WitPackage)
		_ = o.store.AppendAudit(delCtx, &taskstate.AuditEvent{
			TaskID: taskID, StepID: step.ID, EventType: taskstate.EventLinkDeleted,
		})
	}()

	// ── DIRECT PEER LINK (if policy granted direct comms) ────────────────────
	// For the direct case, the orchestrator starts the peer and creates a wRPC
	// link between the two components. The source agent calls the peer directly.
	// The orchestrator tears both links and both components down when done.
	if mode == CommsModeDirected && stepIdx+1 < len(state.Plan) {
		nextStep := state.Plan[stepIdx+1]
		nextComponentID := fmt.Sprintf("%s-%s", taskID, nextStep.AgentType)
		nextImageRef := o.agentImageRef(nextStep.AgentType)

		// Policy must explicitly permit the agent-direct link as well.
		if _, err := o.EvaluateLink(ctx, taskID, step.ID, componentID, nextComponentID, CapAgentDirect); err != nil {
			return fmt.Errorf("policy denied agent-direct link: %w", err)
		}

		nextCap := capabilityForAgent(nextStep.AgentType)
		if _, err := o.EvaluateLink(ctx, taskID, nextStep.ID, nextComponentID, o.providerIDForCap(nextCap), nextCap); err != nil {
			return fmt.Errorf("policy denied peer capability link: %w", err)
		}

		if err := o.ctl.StartComponent(ctx, hostID, nextImageRef, nextComponentID); err != nil {
			return fmt.Errorf("start peer component: %w", err)
		}
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = o.ctl.StopComponent(stopCtx, hostID, nextComponentID)
		}()

		// Peer's capability link (e.g. summarizer → LLM provider).
		peerCapLink := o.linkForStep(nextStep.AgentType, nextComponentID, o.providerIDForCap(nextCap), nextCap)
		if err := o.ctl.PutLink(ctx, peerCapLink); err != nil {
			return fmt.Errorf("put peer capability link: %w", err)
		}
		defer func() {
			delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = o.ctl.DeleteLink(delCtx, nextComponentID, "default", peerCapLink.WitNamespace, peerCapLink.WitPackage)
		}()

		// Agent-to-agent direct wRPC link.
		peerLink := o.linkForStep(step.AgentType, componentID, nextComponentID, CapAgentDirect)
		if err := o.ctl.PutLink(ctx, peerLink); err != nil {
			return fmt.Errorf("put agent-direct link: %w", err)
		}
		defer func() {
			delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = o.ctl.DeleteLink(delCtx, componentID, "default", peerLink.WitNamespace, peerLink.WitPackage)
		}()
	}

	// ── AGENT INVOCATION ──────────────────────────────────────────────────────
	inputPayload := buildStepPayload(state, stepIdx)
	inputContext := buildStepContext(state, stepIdx)
	if err := o.store.PutPayload(ctx, step.InputKey, inputPayload); err != nil {
		return fmt.Errorf("write input payload: %w", err)
	}

	output, err := o.invokeAgent(ctx, componentID, taskID, step.ID, inputPayload, inputContext)
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

	if err := o.store.PutPayload(ctx, step.OutputKey, output); err != nil {
		return fmt.Errorf("write output payload: %w", err)
	}

	fin := time.Now().UTC()
	if err := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		s.Plan[stepIdx].Status = taskstate.StepCompleted
		s.Plan[stepIdx].CompletedAt = &fin
		s.Results[step.OutputKey] = output
		return nil
	}); err != nil {
		return fmt.Errorf("mark step completed: %w", err)
	}

	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID: taskID, StepID: step.ID, EventType: taskstate.EventStepCompleted,
	})
	span.SetStatus(codes.Ok, "step completed")
	log.Info("step completed")
	return nil
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
// Each agent type has its own expected payload shape.
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

// buildStepContext assembles the kv-pair context slice for a step, carrying
// prior step outputs under well-known keys the downstream agent can read.
func buildStepContext(state *taskstate.TaskState, stepIdx int) []agentKVPair {
	var ctx []agentKVPair
	for i := 0; i < stepIdx; i++ {
		s := state.Plan[i]
		if v, ok := state.Results[s.OutputKey]; ok {
			// Map each prior agent output to the canonical context key
			// the downstream agent is expected to read.
			key := contextKeyForAgent(s.AgentType)
			ctx = append(ctx, agentKVPair{Key: key, Val: v})
		}
	}
	return ctx
}

// contextKeyForAgent returns the well-known context key for a given agent's output.
func contextKeyForAgent(agentType string) string {
	switch agentType {
	case "web-search":
		return "web_search_results"
	case "summarizer":
		return "summary_result"
	default:
		return agentType + "_result"
	}
}

// capabilityForAgent returns the primary capability type for a given agent type.
func capabilityForAgent(agentType string) PolicyCapability {
	switch agentType {
	case "web-search":
		return CapHTTP
	case "summarizer":
		return CapLLM
	default:
		return CapHTTP
	}
}
