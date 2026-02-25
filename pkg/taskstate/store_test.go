package taskstate_test

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// connectTestNATS connects to a running NATS server. Tests are skipped if NATS
// is not available. Run `nats-server -js` before executing these tests, or
// use `wash up` which bundles a NATS server.
func connectTestNATS(t *testing.T) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(nats.DefaultURL)
	if err != nil {
		t.Skipf("NATS not available (%v); skipping", err)
	}
	t.Cleanup(func() { nc.Close() })
	return nc
}

func newTestStore(t *testing.T) (*taskstate.Store, context.Context) {
	t.Helper()
	ctx := context.Background()
	nc := connectTestNATS(t)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("jetstream.New: %v", err)
	}
	store, err := taskstate.NewStore(ctx, js)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store, ctx
}

func uniqueID(prefix string) string {
	return prefix + "-" + time.Now().Format("20060102150405.000")
}

func TestPutGet(t *testing.T) {
	store, ctx := newTestStore(t)

	id := uniqueID("test-put-get")
	state := &taskstate.TaskState{
		TaskID:    id,
		Status:    taskstate.StatusPending,
		Plan:      []taskstate.Step{{ID: "s1", AgentType: "web-search", Status: taskstate.StepPending}},
		Results:   map[string]string{},
		Context:   map[string]string{"query": "wasmCloud"},
		CreatedAt: time.Now().UTC(),
	}

	if err := store.Put(ctx, state); err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, id) })

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TaskID != id {
		t.Errorf("TaskID: got %q; want %q", got.TaskID, id)
	}
	if got.Status != taskstate.StatusPending {
		t.Errorf("Status: got %q; want pending", got.Status)
	}
	if len(got.Plan) != 1 || got.Plan[0].AgentType != "web-search" {
		t.Errorf("plan mismatch: %+v", got.Plan)
	}
}

func TestUpdate_CAS(t *testing.T) {
	store, ctx := newTestStore(t)

	id := uniqueID("test-cas")
	state := &taskstate.TaskState{
		TaskID:    id,
		Status:    taskstate.StatusPending,
		Plan:      []taskstate.Step{},
		Results:   map[string]string{},
		Context:   map[string]string{},
		CreatedAt: time.Now().UTC(),
	}
	if err := store.Put(ctx, state); err != nil {
		t.Fatalf("Put: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, id) })

	if err := store.Update(ctx, id, func(s *taskstate.TaskState) error {
		s.Status = taskstate.StatusRunning
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Status != taskstate.StatusRunning {
		t.Errorf("expected StatusRunning after update, got %q", got.Status)
	}
}

func TestAudit(t *testing.T) {
	store, ctx := newTestStore(t)

	event := &taskstate.AuditEvent{
		TaskID:           uniqueID("audit-task"),
		EventType:        taskstate.EventPolicyPermit,
		PolicySource:     "web-search",
		PolicyTarget:     "summarizer",
		PolicyCapability: "http",
		Timestamp:        time.Now().UTC(),
	}
	if err := store.AppendAudit(ctx, event); err != nil {
		t.Fatalf("AppendAudit: %v", err)
	}
}

func TestPayloadRoundtrip(t *testing.T) {
	store, ctx := newTestStore(t)

	key := uniqueID("payload")
	payload := `{"results":[{"title":"Test","url":"https://example.com"}]}`

	if err := store.PutPayload(ctx, key, payload); err != nil {
		t.Fatalf("PutPayload: %v", err)
	}

	got, err := store.GetPayload(ctx, key)
	if err != nil {
		t.Fatalf("GetPayload: %v", err)
	}
	if got != payload {
		t.Errorf("payload mismatch:\ngot:  %s\nwant: %s", got, payload)
	}

	if err := store.DeletePayload(ctx, key); err != nil {
		t.Fatalf("DeletePayload: %v", err)
	}
}

func TestGetMissing(t *testing.T) {
	store, ctx := newTestStore(t)
	_, err := store.Get(ctx, "definitely-does-not-exist-xyz")
	if err == nil {
		t.Error("expected error getting missing key, got nil")
	}
}
