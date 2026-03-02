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

	taskCtx := map[string]string{"query": req.Query, "type": req.Type}
	for k, v := range req.Context {
		taskCtx[k] = v
	}

	// ── SUBMIT POLICY GATE ──────────────────────────────────────────────────
	submitResult, err := o.evaluateSubmitPolicy(r.Context(), req.Type, req.Query, taskCtx)
	if err != nil {
		o.logger.Error("submit policy evaluation failed", "err", err)
		http.Error(w, "policy evaluation error", http.StatusInternalServerError)
		return
	}
	if !submitResult.Permitted {
		msg := "task submission denied by policy"
		if submitResult.DenyMessage != nil {
			msg = *submitResult.DenyMessage
		}
		http.Error(w, msg, http.StatusForbidden)
		return
	}

	taskID := uuid.New().String()

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

// ApproveStepRequest is the JSON body for POST /tasks/{id}/steps/{stepId}/approve.
type ApproveStepRequest struct {
	ApprovedBy string `json:"approved_by"`
}

func (o *Orchestrator) handleApproveStep(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	stepID := r.PathValue("stepId")
	if taskID == "" || stepID == "" {
		http.Error(w, "task id and step id are required", http.StatusBadRequest)
		return
	}

	var req ApproveStepRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.ApprovedBy == "" {
		req.ApprovedBy = "unknown"
	}

	state, err := o.store.Get(r.Context(), taskID)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	idx := stepIndex(state.Plan, stepID)
	if idx < 0 {
		http.Error(w, "step not found", http.StatusNotFound)
		return
	}
	if state.Plan[idx].Status != taskstate.StepAwaitingApproval {
		http.Error(w, fmt.Sprintf("step is not awaiting approval (status: %s)", state.Plan[idx].Status), http.StatusConflict)
		return
	}

	now := time.Now().UTC()
	if err := o.store.Update(r.Context(), taskID, func(s *taskstate.TaskState) error {
		i := stepIndex(s.Plan, stepID)
		if i < 0 {
			return fmt.Errorf("step %s not found", stepID)
		}
		if s.Plan[i].Status != taskstate.StepAwaitingApproval {
			return fmt.Errorf("step is not awaiting approval")
		}
		s.Plan[i].Status = taskstate.StepPending
		s.Plan[i].ApprovedBy = req.ApprovedBy
		s.Plan[i].ApprovedAt = &now
		return nil
	}); err != nil {
		o.logger.Error("approve step failed", "task_id", taskID, "step_id", stepID, "err", err)
		http.Error(w, "failed to approve step", http.StatusInternalServerError)
		return
	}

	_ = o.store.AppendAudit(r.Context(), &taskstate.AuditEvent{
		TaskID:    taskID,
		StepID:    stepID,
		EventType: taskstate.EventStepApproved,
		Message:   fmt.Sprintf("approved by %s", req.ApprovedBy),
	})

	o.logger.Info("step approved", "task_id", taskID, "step_id", stepID, "approved_by", req.ApprovedBy)

	go o.runTask(o.ctx, taskID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"task_id": taskID,
		"step_id": stepID,
		"status":  "approved",
	})
}

// RejectStepRequest is the JSON body for POST /tasks/{id}/steps/{stepId}/reject.
type RejectStepRequest struct {
	RejectedBy string `json:"rejected_by"`
	Reason     string `json:"reason"`
}

func (o *Orchestrator) handleRejectStep(w http.ResponseWriter, r *http.Request) {
	taskID := r.PathValue("id")
	stepID := r.PathValue("stepId")
	if taskID == "" || stepID == "" {
		http.Error(w, "task id and step id are required", http.StatusBadRequest)
		return
	}

	var req RejectStepRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.RejectedBy == "" {
		req.RejectedBy = "unknown"
	}
	if req.Reason == "" {
		req.Reason = "rejected by human reviewer"
	}

	state, err := o.store.Get(r.Context(), taskID)
	if err != nil {
		http.Error(w, "task not found", http.StatusNotFound)
		return
	}

	idx := stepIndex(state.Plan, stepID)
	if idx < 0 {
		http.Error(w, "step not found", http.StatusNotFound)
		return
	}
	if state.Plan[idx].Status != taskstate.StepAwaitingApproval {
		http.Error(w, fmt.Sprintf("step is not awaiting approval (status: %s)", state.Plan[idx].Status), http.StatusConflict)
		return
	}

	if err := o.store.Update(r.Context(), taskID, func(s *taskstate.TaskState) error {
		i := stepIndex(s.Plan, stepID)
		if i < 0 {
			return fmt.Errorf("step %s not found", stepID)
		}
		if s.Plan[i].Status != taskstate.StepAwaitingApproval {
			return fmt.Errorf("step is not awaiting approval")
		}
		s.Plan[i].Status = taskstate.StepDenied
		s.Plan[i].Error = req.Reason
		return nil
	}); err != nil {
		o.logger.Error("reject step failed", "task_id", taskID, "step_id", stepID, "err", err)
		http.Error(w, "failed to reject step", http.StatusInternalServerError)
		return
	}

	_ = o.store.AppendAudit(r.Context(), &taskstate.AuditEvent{
		TaskID:    taskID,
		StepID:    stepID,
		EventType: taskstate.EventStepRejected,
		Message:   fmt.Sprintf("rejected by %s: %s", req.RejectedBy, req.Reason),
	})

	o.logger.Info("step rejected", "task_id", taskID, "step_id", stepID, "rejected_by", req.RejectedBy)

	go o.runTask(o.ctx, taskID)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{
		"task_id": taskID,
		"step_id": stepID,
		"status":  "rejected",
	})
}

// PendingApproval is returned by the list approvals endpoint.
type PendingApproval struct {
	StepID    string `json:"step_id"`
	AgentType string `json:"agent_type"`
	Reason    string `json:"approval_reason"`
	Status    string `json:"status"`
}

func (o *Orchestrator) handleListApprovals(w http.ResponseWriter, r *http.Request) {
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

	var pending []PendingApproval
	for _, s := range state.Plan {
		if s.Status == taskstate.StepAwaitingApproval {
			pending = append(pending, PendingApproval{
				StepID:    s.ID,
				AgentType: s.AgentType,
				Reason:    s.ApprovalReason,
				Status:    string(s.Status),
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pending)
}
