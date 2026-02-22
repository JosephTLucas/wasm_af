package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// SubmitTaskRequest is the JSON body for POST /tasks.
type SubmitTaskRequest struct {
	// Type is the top-level task category (e.g. "research").
	Type string `json:"type"`
	// Query is the user-supplied query string.
	Query string `json:"query"`
	// Context is optional additional KV context to inject into the task.
	Context map[string]string `json:"context,omitempty"`
}

// SubmitTaskResponse is returned on 202 Accepted.
type SubmitTaskResponse struct {
	TaskID string `json:"task_id"`
}

// handleSubmitTask accepts a new task, persists its initial state, and
// dispatches an asynchronous goroutine to run the agent loop.
func (o *Orchestrator) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "orchestrator.submit_task")
	defer span.End()

	var req SubmitTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if req.Type == "" || req.Query == "" {
		http.Error(w, "type and query are required", http.StatusBadRequest)
		return
	}

	taskID := uuid.New().String()
	span.SetAttributes(
		attribute.String("task.id", taskID),
		attribute.String("task.type", req.Type),
	)

	// Build the initial plan based on task type.
	plan, err := buildPlan(req.Type, taskID)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		http.Error(w, fmt.Sprintf("unsupported task type: %s", req.Type), http.StatusUnprocessableEntity)
		return
	}

	ctx2 := map[string]string{"query": req.Query}
	for k, v := range req.Context {
		ctx2[k] = v
	}

	state := &taskstate.TaskState{
		TaskID:    taskID,
		Status:    taskstate.StatusPending,
		Plan:      plan,
		Results:   make(map[string]string),
		Context:   ctx2,
		CreatedAt: time.Now().UTC(),
	}

	if err := o.store.Put(ctx, state); err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		o.logger.Error("failed to persist initial task state", "task_id", taskID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID:    taskID,
		EventType: taskstate.EventTaskCreated,
		Message:   fmt.Sprintf("task created, type=%s query=%s", req.Type, req.Query),
	}); err != nil {
		o.logger.Warn("failed to write task.created audit event", "task_id", taskID, "err", err)
	}

	// Run the agent loop asynchronously.
	go o.runTask(context.Background(), taskID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(SubmitTaskResponse{TaskID: taskID})
}

// handleGetTask returns the current state of a task.
func (o *Orchestrator) handleGetTask(w http.ResponseWriter, r *http.Request) {
	ctx, span := tracer.Start(r.Context(), "orchestrator.get_task")
	defer span.End()

	taskID := r.PathValue("id")
	if taskID == "" {
		http.Error(w, "missing task id", http.StatusBadRequest)
		return
	}

	span.SetAttributes(attribute.String("task.id", taskID))

	state, err := o.store.Get(ctx, taskID)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

// buildPlan returns the ordered list of steps for the given task type.
// Step IDs are deterministic: "<task-id>-step-<n>".
func buildPlan(taskType, taskID string) ([]taskstate.Step, error) {
	stepID := func(n int) string {
		return fmt.Sprintf("%s-step-%d", taskID, n)
	}

	switch taskType {
	case "research":
		return []taskstate.Step{
			{
				ID:        stepID(1),
				AgentType: "web-search",
				InputKey:  stepID(1) + ".input",
				OutputKey: stepID(1) + ".output",
				Status:    taskstate.StepPending,
			},
			{
				ID:        stepID(2),
				AgentType: "summarizer",
				InputKey:  stepID(2) + ".input",
				OutputKey: stepID(2) + ".output",
				Status:    taskstate.StepPending,
			},
		}, nil
	default:
		return nil, fmt.Errorf("unknown task type %q", taskType)
	}
}

// traceSpan is a no-op fallback when OTel is not configured.
var tracer trace.Tracer = trace.NewNoopTracerProvider().Tracer("orchestrator")
