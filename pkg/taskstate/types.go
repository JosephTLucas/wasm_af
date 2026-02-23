// Package taskstate manages the lifecycle and persistence of orchestrator task state
// in NATS JetStream KV. All mutations go through optimistic CAS to prevent lost updates.
package taskstate

import "time"

// Status represents the lifecycle state of a task.
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// StepStatus tracks the outcome of a single plan step.
type StepStatus string

const (
	StepPending   StepStatus = "pending"
	StepRunning   StepStatus = "running"
	StepCompleted StepStatus = "completed"
	StepFailed    StepStatus = "failed"
	StepDenied    StepStatus = "denied" // policy evaluation denied the link
)

// CommsMode mirrors the WIT policy permit comms-mode enum.
type CommsMode string

const (
	CommsModeMediated CommsMode = "mediated"
	CommsModeDirected CommsMode = "direct"
)

// Step is one unit of work within a task plan.
type Step struct {
	ID          string     `json:"id"`
	AgentType   string     `json:"agent_type"`   // e.g. "web-search", "summarizer", "url-fetch"
	InputKey    string     `json:"input_key"`    // KV key holding this step's input payload
	OutputKey   string     `json:"output_key"`   // KV key where result will be written
	Status      StepStatus `json:"status"`
	CommsMode   CommsMode  `json:"comms_mode,omitempty"`
	Error       string     `json:"error,omitempty"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`

	// Group tags steps for parallel execution. Steps with the same non-empty
	// Group value that are contiguous in the plan are started concurrently.
	Group string `json:"group,omitempty"`

	// AllowedHosts overrides the orchestrator-level allowed_hosts for this
	// step's HTTP capability link. Used to scope each parallel fetch instance
	// to a single domain.
	AllowedHosts string `json:"allowed_hosts,omitempty"`

	// Params carries step-specific parameters that buildStepPayload reads.
	// For url-fetch steps this contains {"url": "https://..."}.
	Params map[string]string `json:"params,omitempty"`
}

// TaskState is the authoritative record for a running or completed task.
// Stored in NATS JetStream KV under bucket "wasm-af-tasks", key = TaskID.
type TaskState struct {
	TaskID      string            `json:"task_id"`
	Status      Status            `json:"status"`
	Plan        []Step            `json:"plan"`
	CurrentStep int               `json:"current_step"` // index into Plan
	Results     map[string]string `json:"results"`      // step output key → payload
	Context     map[string]string `json:"context"`      // arbitrary task-level KV
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	Error       string            `json:"error,omitempty"` // terminal error message
}

// AuditEvent is an immutable record appended to the audit log bucket for every
// policy decision and significant lifecycle event. The bucket is configured as
// an append-only stream with a history that is never overwritten.
type AuditEvent struct {
	TaskID    string    `json:"task_id"`
	StepID    string    `json:"step_id,omitempty"`
	EventType EventType `json:"event_type"`
	Timestamp time.Time `json:"timestamp"`

	// Policy decision fields (non-empty for PolicyPermit / PolicyDeny events).
	PolicySource     string `json:"policy_source,omitempty"`
	PolicyTarget     string `json:"policy_target,omitempty"`
	PolicyCapability string `json:"policy_capability,omitempty"`
	PolicyCommsMode  string `json:"policy_comms_mode,omitempty"`
	PolicyDenyCode   string `json:"policy_deny_code,omitempty"`
	PolicyDenyMsg    string `json:"policy_deny_msg,omitempty"`

	// Lifecycle fields.
	ComponentID  string `json:"component_id,omitempty"`
	ComponentRef string `json:"component_ref,omitempty"`
	Message      string `json:"message,omitempty"`
}

// EventType classifies audit events for filtering and dashboards.
type EventType string

const (
	EventTaskCreated    EventType = "task.created"
	EventTaskCompleted  EventType = "task.completed"
	EventTaskFailed     EventType = "task.failed"
	EventStepStarted    EventType = "step.started"
	EventStepCompleted  EventType = "step.completed"
	EventStepFailed     EventType = "step.failed"
	EventPolicyPermit   EventType = "policy.permit"
	EventPolicyDeny     EventType = "policy.deny"
	EventComponentStart EventType = "component.start"
	EventComponentStop  EventType = "component.stop"
	EventLinkCreated    EventType = "link.created"
	EventLinkDeleted    EventType = "link.deleted"
)
