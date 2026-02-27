package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/jolucas/wasm-af/pkg/taskstate"
)

// mockStore is an in-memory TaskStore for unit testing.
type mockStore struct {
	mu       sync.Mutex
	tasks    map[string]*taskstate.TaskState
	audits   []taskstate.AuditEvent
	payloads map[string]string
}

func newMockStore() *mockStore {
	return &mockStore{
		tasks:    make(map[string]*taskstate.TaskState),
		payloads: make(map[string]string),
	}
}

func (m *mockStore) Put(_ context.Context, state *taskstate.TaskState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *state
	m.tasks[state.TaskID] = &cp
	return nil
}

func (m *mockStore) Get(_ context.Context, taskID string) (*taskstate.TaskState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.tasks[taskID]
	if !ok {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	cp := *s
	cp.Plan = make([]taskstate.Step, len(s.Plan))
	copy(cp.Plan, s.Plan)
	if s.Results != nil {
		cp.Results = make(map[string]string, len(s.Results))
		for k, v := range s.Results {
			cp.Results[k] = v
		}
	}
	return &cp, nil
}

func (m *mockStore) Update(_ context.Context, taskID string, fn func(*taskstate.TaskState) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %q not found", taskID)
	}
	return fn(s)
}

func (m *mockStore) Delete(_ context.Context, taskID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.tasks, taskID)
	return nil
}

func (m *mockStore) AppendAudit(_ context.Context, event *taskstate.AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.audits = append(m.audits, *event)
	return nil
}

func (m *mockStore) PutPayload(_ context.Context, key, payload string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.payloads[key] = payload
	return nil
}

func (m *mockStore) GetPayload(_ context.Context, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.payloads[key]
	if !ok {
		return "", fmt.Errorf("payload %q not found", key)
	}
	return v, nil
}

func (m *mockStore) DeletePayload(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.payloads, key)
	return nil
}

func (m *mockStore) auditEvents() []taskstate.AuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]taskstate.AuditEvent, len(m.audits))
	copy(out, m.audits)
	return out
}
