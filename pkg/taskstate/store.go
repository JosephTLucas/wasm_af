package taskstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	// BucketTasks holds serialized TaskState values. Key = task ID.
	BucketTasks = "wasm-af-tasks"
	// BucketAudit is an append-only audit log. Keys are
	// "<task-id>.<unix-nano>" to maintain ordering within a task.
	BucketAudit = "wasm-af-audit"
	// BucketPayloads holds step input/output payloads separately from task
	// state to avoid bloating the state KV entry. Key = step output key.
	BucketPayloads = "wasm-af-payloads"
)

// Store provides task state operations backed by NATS JetStream KV.
type Store struct {
	tasks    jetstream.KeyValue
	audit    jetstream.KeyValue
	payloads jetstream.KeyValue
}

// NewStore creates or updates the three JetStream KV buckets and returns a Store.
// It is safe to call repeatedly — existing buckets are re-used via CreateOrUpdateKeyValue.
func NewStore(ctx context.Context, js jetstream.JetStream) (*Store, error) {
	tasks, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:      BucketTasks,
		Description: "wasm-af task states",
		History:     10, // retain 10 revisions per task for CAS
	})
	if err != nil {
		return nil, fmt.Errorf("tasks bucket: %w", err)
	}

	audit, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       BucketAudit,
		Description:  "wasm-af immutable audit log",
		History:      1,        // each key is unique; no need for history
		MaxValueSize: 64 << 10, // 64 KB per audit entry
	})
	if err != nil {
		return nil, fmt.Errorf("audit bucket: %w", err)
	}

	payloads, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       BucketPayloads,
		Description:  "wasm-af step input/output payloads",
		History:      1,
		MaxValueSize: 4 << 20, // 4 MB per payload
	})
	if err != nil {
		return nil, fmt.Errorf("payloads bucket: %w", err)
	}

	return &Store{tasks: tasks, audit: audit, payloads: payloads}, nil
}

// Put writes a TaskState. The state's UpdatedAt is always set to now.
func (s *Store) Put(ctx context.Context, state *TaskState) error {
	state.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	_, err = s.tasks.Put(ctx, state.TaskID, b)
	return err
}

// Get retrieves the current TaskState for the given task ID.
func (s *Store) Get(ctx context.Context, taskID string) (*TaskState, error) {
	entry, err := s.tasks.Get(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("kv get %s: %w", taskID, err)
	}
	var state TaskState
	if err := json.Unmarshal(entry.Value(), &state); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	return &state, nil
}

// Update performs a read-modify-write with optimistic CAS.
// fn is called with a mutable copy of the current state. If it returns an error,
// Update aborts without writing. If a concurrent writer wins the CAS race,
// Update retries up to 5 times before returning an error.
func (s *Store) Update(ctx context.Context, taskID string, fn func(*TaskState) error) error {
	const maxRetries = 5

	for i := range maxRetries {
		entry, err := s.tasks.Get(ctx, taskID)
		if err != nil {
			return fmt.Errorf("cas read (attempt %d): %w", i+1, err)
		}

		var state TaskState
		if err := json.Unmarshal(entry.Value(), &state); err != nil {
			return fmt.Errorf("unmarshal: %w", err)
		}

		if err := fn(&state); err != nil {
			return err // caller signalled abort
		}

		state.UpdatedAt = time.Now().UTC()
		b, err := json.Marshal(&state)
		if err != nil {
			return fmt.Errorf("marshal: %w", err)
		}

		_, err = s.tasks.Update(ctx, taskID, b, entry.Revision())
		if err == nil {
			return nil
		}
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return fmt.Errorf("cas write: %w", err)
		}
		// CAS conflict — retry
	}
	return fmt.Errorf("too many CAS conflicts for task %s", taskID)
}

// Delete removes a task state entry.
func (s *Store) Delete(ctx context.Context, taskID string) error {
	return s.tasks.Delete(ctx, taskID)
}

// AppendAudit writes an immutable audit event.
// The key is "<task-id>.<unix-nano>" to guarantee ordering per task.
func (s *Store) AppendAudit(ctx context.Context, event *AuditEvent) error {
	if event.Timestamp.IsZero() {
		event.Timestamp = time.Now().UTC()
	}
	b, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal audit: %w", err)
	}
	key := fmt.Sprintf("%s.%d", event.TaskID, event.Timestamp.UnixNano())
	_, err = s.audit.Put(ctx, key, b)
	return err
}

// PutPayload stores a step payload under the given key.
func (s *Store) PutPayload(ctx context.Context, key, payload string) error {
	_, err := s.payloads.Put(ctx, key, []byte(payload))
	return err
}

// GetPayload retrieves a step payload by key.
func (s *Store) GetPayload(ctx context.Context, key string) (string, error) {
	entry, err := s.payloads.Get(ctx, key)
	if err != nil {
		return "", fmt.Errorf("payload get %s: %w", key, err)
	}
	return string(entry.Value()), nil
}

// DeletePayload removes a payload entry after the step has been processed.
func (s *Store) DeletePayload(ctx context.Context, key string) error {
	return s.payloads.Delete(ctx, key)
}
