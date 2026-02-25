package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// SubmitTaskRequest is the JSON body for POST /tasks.
type SubmitTaskRequest struct {
	Type    string            `json:"type"`
	Query   string            `json:"query"`
	Context map[string]string `json:"context,omitempty"`
}

// SubmitTaskResponse is returned on 202 Accepted.
type SubmitTaskResponse struct {
	TaskID string `json:"task_id"`
}

func (o *Orchestrator) handleSubmitTask(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
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

	taskCtx := map[string]string{"query": req.Query}
	for k, v := range req.Context {
		taskCtx[k] = v
	}

	plan, err := o.builders.Build(req.Type, taskID, taskCtx, o.registry, o)
	if err != nil {
		http.Error(w, fmt.Sprintf("unsupported task type: %s", req.Type), http.StatusUnprocessableEntity)
		return
	}

	state := &taskstate.TaskState{
		TaskID:    taskID,
		Status:    taskstate.StatusPending,
		Plan:      plan,
		Results:   make(map[string]string),
		Context:   taskCtx,
		CreatedAt: time.Now().UTC(),
	}

	ctx := r.Context()
	if err := o.store.Put(ctx, state); err != nil {
		o.logger.Error("failed to persist initial task state", "task_id", taskID, "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if err := o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID:    taskID,
		EventType: taskstate.EventTaskCreated,
		Message:   fmt.Sprintf("task created, type=%s query=%s", req.Type, req.Query),
	}); err != nil {
		o.logger.Error("audit write failed", "task_id", taskID, "event", taskstate.EventTaskCreated, "err", err)
	}

	go o.runTask(o.ctx, taskID)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(SubmitTaskResponse{TaskID: taskID})
}

func (o *Orchestrator) handleGetTask(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	if taskID == "" {
		http.Error(w, "missing task id", http.StatusBadRequest)
		return
	}

	state, err := o.store.Get(r.Context(), taskID)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(state)
}

