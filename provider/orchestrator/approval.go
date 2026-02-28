package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// ApprovalEvent is the JSON payload published to NATS and/or POSTed to the
// webhook when a step requires human approval before execution.
type ApprovalEvent struct {
	TaskID    string `json:"task_id"`
	StepID    string `json:"step_id"`
	AgentType string `json:"agent_type"`
	Reason    string `json:"reason"`
	Timestamp string `json:"timestamp"`
}

// publishApprovalNeeded sends an approval event to NATS and, if configured,
// to the webhook callback URL. Errors are logged but do not block the caller.
func (o *Orchestrator) publishApprovalNeeded(ctx context.Context, taskID, stepID, agentType, reason string) {
	event := ApprovalEvent{
		TaskID:    taskID,
		StepID:    stepID,
		AgentType: agentType,
		Reason:    reason,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	payload, err := json.Marshal(event)
	if err != nil {
		o.logger.Error("marshal approval event", "err", err)
		return
	}

	if o.natsConn != nil {
		subject := fmt.Sprintf("wasm-af.approvals.%s", taskID)
		if pubErr := o.natsConn.Publish(subject, payload); pubErr != nil {
			o.logger.Error("publish approval event to NATS", "subject", subject, "err", pubErr)
		} else {
			o.logger.Info("approval event published", "subject", subject, "step_id", stepID)
		}
	}

	if o.approvalWebhookURL != "" {
		go o.postApprovalWebhook(ctx, payload)
	}

	if o.approvalTimeoutSec > 0 {
		go o.startApprovalTimeout(taskID, stepID, o.approvalTimeoutSec)
	}
}

func (o *Orchestrator) postApprovalWebhook(ctx context.Context, payload []byte) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, o.approvalWebhookURL, bytes.NewReader(payload))
	if err != nil {
		o.logger.Error("build approval webhook request", "err", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		o.logger.Error("approval webhook request failed", "url", o.approvalWebhookURL, "err", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode >= 300 {
		o.logger.Warn("approval webhook non-2xx response", "url", o.approvalWebhookURL, "status", resp.StatusCode)
	}
}

// startApprovalTimeout auto-rejects a step if no approval arrives within the
// configured timeout. Runs in its own goroutine.
func (o *Orchestrator) startApprovalTimeout(taskID, stepID string, timeoutSec int) {
	time.Sleep(time.Duration(timeoutSec) * time.Second)

	ctx := o.ctx
	state, err := o.store.Get(ctx, taskID)
	if err != nil {
		return
	}

	idx := stepIndex(state.Plan, stepID)
	if idx < 0 {
		return
	}
	if state.Plan[idx].Status != taskstate.StepAwaitingApproval {
		return
	}

	if err := o.store.Update(ctx, taskID, func(s *taskstate.TaskState) error {
		i := stepIndex(s.Plan, stepID)
		if i < 0 {
			return nil
		}
		if s.Plan[i].Status != taskstate.StepAwaitingApproval {
			return nil
		}
		s.Plan[i].Status = taskstate.StepDenied
		s.Plan[i].Error = "approval timed out"
		return nil
	}); err != nil {
		o.logger.Error("approval timeout update failed", "task_id", taskID, "step_id", stepID, "err", err)
		return
	}

	_ = o.store.AppendAudit(ctx, &taskstate.AuditEvent{
		TaskID:    taskID,
		StepID:    stepID,
		EventType: taskstate.EventStepRejected,
		Message:   "approval timed out",
	})
	o.logger.Info("step auto-rejected (approval timeout)", "task_id", taskID, "step_id", stepID)

	go o.runTask(ctx, taskID)
}
