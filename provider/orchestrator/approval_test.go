package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

func testOrchestratorWithApprovalPolicy(store taskstate.TaskStore) *Orchestrator {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, _ := ParseAgentRegistry([]byte(`{
		"email-send": {
			"wasm_name": "email_send",
			"capability": "email",
			"context_key": "email_result",
			"host_functions": ["send_email"],
			"payload_fields": {}
		},
		"shell": {
			"wasm_name": "shell",
			"capability": "shell",
			"context_key": "shell_result",
			"host_functions": ["exec_command"],
			"payload_fields": {}
		},
		"router": {
			"wasm_name": "router",
			"capability": "llm",
			"context_key": "router_result",
			"host_functions": ["llm_complete"],
			"payload_fields": {}
		}
	}`))
	builders := NewPlanBuilderRegistry()
	RegisterDefaultBuilders(builders)

	modules := map[string]string{
		"authz.rego": `package wasm_af.authz
import rego.v1
default allow := true
default requires_approval := false
requires_approval if { input.step.agent_type == "email-send" }
approval_reason := "email requires human approval" if { input.step.agent_type == "email-send" }
`,
		"submit.rego": `package wasm_af.submit
import rego.v1
default allow := true
`,
	}
	policy, _ := NewOPAEvaluator(context.Background(), modules, nil)

	return &Orchestrator{
		logger:               logger,
		store:                store,
		wasmDir:              "/tmp/nonexistent-wasm-dir",
		policy:               policy,
		registry:             reg,
		builders:             builders,
		hostFns:              NewHostFnRegistry(logger),
		ctx:                  context.Background(),
		pluginTimeout:        30 * time.Second,
		pluginMaxMemoryPages: 256,
		pluginMaxHTTPBytes:   4 << 20,
	}
}

func seedTaskWithStep(store *mockStore, taskID, stepID, agentType string, status taskstate.StepStatus) {
	state := &taskstate.TaskState{
		TaskID: taskID,
		Status: taskstate.StatusRunning,
		Plan: []taskstate.Step{
			{
				ID:        stepID,
				AgentType: agentType,
				InputKey:  stepID + ".input",
				OutputKey: stepID + ".output",
				Status:    status,
			},
		},
		Results: make(map[string]string),
		Context: map[string]string{"type": "test"},
	}
	_ = store.Put(context.Background(), state)
}

func TestApproveStep_Success(t *testing.T) {
	store := newMockStore()
	orch := testOrchestratorWithApprovalPolicy(store)

	seedTaskWithStep(store, "task-1", "step-1", "email-send", taskstate.StepAwaitingApproval)

	body := `{"approved_by": "alice"}`
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/steps/step-1/approve", strings.NewReader(body))
	req.SetPathValue("id", "task-1")
	req.SetPathValue("stepId", "step-1")
	w := httptest.NewRecorder()
	orch.handleApproveStep(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&resp)
	if resp["status"] != "approved" {
		t.Errorf("expected status approved, got %q", resp["status"])
	}

	time.Sleep(10 * time.Millisecond)
	state, _ := store.Get(context.Background(), "task-1")
	if state.Plan[0].Status != taskstate.StepPending {
		// runTask will immediately try to run the step and may change status;
		// but the CAS update should have set it to pending first.
		// Since we don't have real WASM, runTask will fail the step, but
		// the approval itself should have worked. Check audit events instead.
	}

	events := store.auditEvents()
	found := false
	for _, e := range events {
		if e.EventType == taskstate.EventStepApproved && e.StepID == "step-1" {
			found = true
			if !strings.Contains(e.Message, "alice") {
				t.Errorf("audit message should mention approver, got %q", e.Message)
			}
		}
	}
	if !found {
		t.Error("expected step.approved audit event")
	}
}

func TestApproveStep_WrongStatus(t *testing.T) {
	store := newMockStore()
	orch := testOrchestratorWithApprovalPolicy(store)

	seedTaskWithStep(store, "task-1", "step-1", "email-send", taskstate.StepRunning)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-1/steps/step-1/approve", strings.NewReader(`{}`))
	req.SetPathValue("id", "task-1")
	req.SetPathValue("stepId", "step-1")
	w := httptest.NewRecorder()
	orch.handleApproveStep(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for non-awaiting step, got %d", w.Code)
	}
}

func TestApproveStep_TaskNotFound(t *testing.T) {
	store := newMockStore()
	orch := testOrchestratorWithApprovalPolicy(store)

	req := httptest.NewRequest(http.MethodPost, "/tasks/nonexistent/steps/step-1/approve", strings.NewReader(`{}`))
	req.SetPathValue("id", "nonexistent")
	req.SetPathValue("stepId", "step-1")
	w := httptest.NewRecorder()
	orch.handleApproveStep(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestRejectStep_Success(t *testing.T) {
	store := newMockStore()
	orch := testOrchestratorWithApprovalPolicy(store)

	seedTaskWithStep(store, "task-2", "step-2", "email-send", taskstate.StepAwaitingApproval)

	body := `{"rejected_by": "bob", "reason": "not appropriate"}`
	req := httptest.NewRequest(http.MethodPost, "/tasks/task-2/steps/step-2/reject", strings.NewReader(body))
	req.SetPathValue("id", "task-2")
	req.SetPathValue("stepId", "step-2")
	w := httptest.NewRecorder()
	orch.handleRejectStep(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	time.Sleep(10 * time.Millisecond)
	state, _ := store.Get(context.Background(), "task-2")
	if state.Plan[0].Status != taskstate.StepDenied {
		t.Errorf("expected step denied after rejection, got %s", state.Plan[0].Status)
	}
	if state.Plan[0].Error != "not appropriate" {
		t.Errorf("expected rejection reason in error, got %q", state.Plan[0].Error)
	}

	events := store.auditEvents()
	found := false
	for _, e := range events {
		if e.EventType == taskstate.EventStepRejected && e.StepID == "step-2" {
			found = true
		}
	}
	if !found {
		t.Error("expected step.rejected audit event")
	}
}

func TestRejectStep_WrongStatus(t *testing.T) {
	store := newMockStore()
	orch := testOrchestratorWithApprovalPolicy(store)

	seedTaskWithStep(store, "task-2", "step-2", "email-send", taskstate.StepCompleted)

	req := httptest.NewRequest(http.MethodPost, "/tasks/task-2/steps/step-2/reject", strings.NewReader(`{}`))
	req.SetPathValue("id", "task-2")
	req.SetPathValue("stepId", "step-2")
	w := httptest.NewRecorder()
	orch.handleRejectStep(w, req)

	if w.Code != http.StatusConflict {
		t.Errorf("expected 409 for non-awaiting step, got %d", w.Code)
	}
}

func TestListApprovals_ReturnsOnlyAwaitingSteps(t *testing.T) {
	store := newMockStore()
	orch := testOrchestratorWithApprovalPolicy(store)

	state := &taskstate.TaskState{
		TaskID: "task-3",
		Status: taskstate.StatusRunning,
		Plan: []taskstate.Step{
			{ID: "s1", AgentType: "router", Status: taskstate.StepCompleted},
			{ID: "s2", AgentType: "email-send", Status: taskstate.StepAwaitingApproval, ApprovalReason: "email requires approval"},
			{ID: "s3", AgentType: "shell", Status: taskstate.StepPending},
		},
		Results: make(map[string]string),
		Context: map[string]string{},
	}
	_ = store.Put(context.Background(), state)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-3/approvals", nil)
	req.SetPathValue("id", "task-3")
	w := httptest.NewRecorder()
	orch.handleListApprovals(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var pending []PendingApproval
	_ = json.NewDecoder(w.Body).Decode(&pending)
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending approval, got %d", len(pending))
	}
	if pending[0].StepID != "s2" {
		t.Errorf("expected step s2, got %s", pending[0].StepID)
	}
	if pending[0].AgentType != "email-send" {
		t.Errorf("expected agent type email-send, got %s", pending[0].AgentType)
	}
	if pending[0].Reason != "email requires approval" {
		t.Errorf("expected reason, got %q", pending[0].Reason)
	}
}

func TestListApprovals_EmptyWhenNoneAwaiting(t *testing.T) {
	store := newMockStore()
	orch := testOrchestratorWithApprovalPolicy(store)

	state := &taskstate.TaskState{
		TaskID: "task-4",
		Status: taskstate.StatusCompleted,
		Plan: []taskstate.Step{
			{ID: "s1", AgentType: "router", Status: taskstate.StepCompleted},
		},
		Results: make(map[string]string),
		Context: map[string]string{},
	}
	_ = store.Put(context.Background(), state)

	req := httptest.NewRequest(http.MethodGet, "/tasks/task-4/approvals", nil)
	req.SetPathValue("id", "task-4")
	w := httptest.NewRecorder()
	orch.handleListApprovals(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var pending []PendingApproval
	_ = json.NewDecoder(w.Body).Decode(&pending)
	if pending == nil {
		// JSON null is fine — we just need to not crash
		return
	}
	if len(pending) != 0 {
		t.Errorf("expected 0 pending approvals, got %d", len(pending))
	}
}

func TestPolicyResult_RequiresApproval(t *testing.T) {
	modules := map[string]string{
		"authz.rego": `package wasm_af.authz
import rego.v1
default allow := true
default requires_approval := false
requires_approval if { input.step.agent_type == "email-send" }
approval_reason := "email needs approval" if { input.step.agent_type == "email-send" }
`,
	}
	eval, err := NewOPAEvaluator(context.Background(), modules, nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := eval.EvaluateStep(context.Background(), map[string]any{
		"step": map[string]any{
			"agent_type": "email-send",
			"params":     map[string]any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Permitted {
		t.Error("expected step to be permitted")
	}
	if !result.RequiresApproval {
		t.Error("expected requires_approval to be true")
	}
	if result.ApprovalReason != "email needs approval" {
		t.Errorf("expected approval reason, got %q", result.ApprovalReason)
	}
}

func TestPolicyResult_NoApprovalForSafeAgent(t *testing.T) {
	modules := map[string]string{
		"authz.rego": `package wasm_af.authz
import rego.v1
default allow := true
default requires_approval := false
requires_approval if { input.step.agent_type == "email-send" }
`,
	}
	eval, err := NewOPAEvaluator(context.Background(), modules, nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := eval.EvaluateStep(context.Background(), map[string]any{
		"step": map[string]any{
			"agent_type": "router",
			"params":     map[string]any{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Permitted {
		t.Error("expected step to be permitted")
	}
	if result.RequiresApproval {
		t.Error("expected requires_approval to be false for router")
	}
}
