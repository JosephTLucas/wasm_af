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

func testOrchestrator(store taskstate.TaskStore) *Orchestrator {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg, _ := ParseAgentRegistry([]byte(`{
		"test-agent": {
			"wasm_name": "test_agent",
			"capability": "test",
			"context_key": "test_result",
			"host_functions": [],
			"payload_fields": {}
		}
	}`))
	builders := NewPlanBuilderRegistry()
	RegisterDefaultBuilders(builders)
	builders.Register("chat", ChatBuilder{})
	builders.Register("email-reply", EmailReplyBuilder{})

	modules := map[string]string{
		"authz.rego": `package wasm_af.authz
import rego.v1
default allow := true
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

func TestHandleSubmitTask_MissingBody(t *testing.T) {
	store := newMockStore()
	orch := testOrchestrator(store)

	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(""))
	w := httptest.NewRecorder()
	orch.handleSubmitTask(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmitTask_MissingFields(t *testing.T) {
	store := newMockStore()
	orch := testOrchestrator(store)

	req := httptest.NewRequest(http.MethodPost, "/tasks",
		strings.NewReader(`{"type":"","query":""}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	orch.handleSubmitTask(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestHandleSubmitTask_UnknownType(t *testing.T) {
	store := newMockStore()
	orch := testOrchestrator(store)

	req := httptest.NewRequest(http.MethodPost, "/tasks",
		strings.NewReader(`{"type":"nonexistent-type","query":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	orch.handleSubmitTask(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Errorf("expected 422 for unknown type, got %d", w.Code)
	}
}

func TestHandleSubmitTask_ValidRequest(t *testing.T) {
	store := newMockStore()
	orch := testOrchestrator(store)

	body := `{"type":"fan-out-summarizer","query":"compare","context":{"urls":"https://a.com,https://b.com"}}`
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	orch.handleSubmitTask(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var resp SubmitTaskResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.TaskID == "" {
		t.Error("expected non-empty task ID")
	}

	// Verify task was persisted
	time.Sleep(10 * time.Millisecond)
	state, err := store.Get(context.Background(), resp.TaskID)
	if err != nil {
		t.Fatalf("task not found in store: %v", err)
	}
	if len(state.Plan) != 3 {
		t.Errorf("expected 3 plan steps (2 fetch + 1 summarizer), got %d", len(state.Plan))
	}
}

func TestHandleSubmitTask_AuditEventCreated(t *testing.T) {
	store := newMockStore()
	orch := testOrchestrator(store)

	body := `{"type":"research","query":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	orch.handleSubmitTask(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	time.Sleep(10 * time.Millisecond)
	events := store.auditEvents()
	found := false
	for _, e := range events {
		if e.EventType == taskstate.EventTaskCreated {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected task.created audit event")
	}
}

func TestHandleGetTask_Found(t *testing.T) {
	store := newMockStore()
	orch := testOrchestrator(store)

	state := &taskstate.TaskState{
		TaskID:  "test-123",
		Status:  taskstate.StatusCompleted,
		Plan:    []taskstate.Step{},
		Results: map[string]string{"out": "done"},
		Context: map[string]string{},
	}
	_ = store.Put(context.Background(), state)

	req := httptest.NewRequest(http.MethodGet, "/tasks/test-123", nil)
	req.SetPathValue("id", "test-123")
	w := httptest.NewRecorder()
	orch.handleGetTask(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var got taskstate.TaskState
	if err := json.NewDecoder(w.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TaskID != "test-123" {
		t.Errorf("task ID mismatch: %q", got.TaskID)
	}
	if got.Status != taskstate.StatusCompleted {
		t.Errorf("status mismatch: %q", got.Status)
	}
}

func TestHandleGetTask_NotFound(t *testing.T) {
	store := newMockStore()
	orch := testOrchestrator(store)

	req := httptest.NewRequest(http.MethodGet, "/tasks/nonexistent", nil)
	req.SetPathValue("id", "nonexistent")
	w := httptest.NewRecorder()
	orch.handleGetTask(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

func TestHandleGetTask_MissingID(t *testing.T) {
	store := newMockStore()
	orch := testOrchestrator(store)

	req := httptest.NewRequest(http.MethodGet, "/tasks/", nil)
	w := httptest.NewRecorder()
	orch.handleGetTask(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing ID, got %d", w.Code)
	}
}

func TestHandleSubmitTask_PolicyDeny(t *testing.T) {
	store := newMockStore()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	modules := map[string]string{
		"authz.rego": `package wasm_af.authz
import rego.v1
default allow := true
`,
		"submit.rego": `package wasm_af.submit
import rego.v1
default allow := false
deny_message := "not allowed"
`,
	}
	policy, _ := NewOPAEvaluator(context.Background(), modules, nil)

	orch := &Orchestrator{
		logger:   logger,
		store:    store,
		policy:   policy,
		registry: &AgentRegistry{agents: map[string]*AgentMeta{}},
		builders: NewPlanBuilderRegistry(),
		hostFns:  NewHostFnRegistry(logger),
		ctx:      context.Background(),
	}

	body := `{"type":"anything","query":"test"}`
	req := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	orch.handleSubmitTask(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}
