use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Status {
    Pending,
    Running,
    Completed,
    Failed,
    AwaitingApproval,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StepStatus {
    #[default]
    Pending,
    Running,
    Completed,
    Failed,
    Denied,
    AwaitingApproval,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct Step {
    pub id: String,
    pub agent_type: String,
    pub input_key: String,
    pub output_key: String,
    pub status: StepStatus,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub started_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub completed_at: Option<DateTime<Utc>>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub depends_on: Vec<String>,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    pub params: HashMap<String, String>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approval_reason: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approved_by: String,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub approved_at: Option<DateTime<Utc>>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TaskState {
    pub task_id: String,
    pub status: Status,
    pub plan: Vec<Step>,
    #[serde(default)]
    pub current_step: usize,
    #[serde(default)]
    pub results: HashMap<String, String>,
    #[serde(default)]
    pub context: HashMap<String, String>,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub error: String,
}

#[derive(Debug, Clone, Default, PartialEq, Eq, Serialize, Deserialize)]
pub enum EventType {
    #[default]
    #[serde(rename = "task.created")]
    TaskCreated,
    #[serde(rename = "task.completed")]
    TaskCompleted,
    #[serde(rename = "task.failed")]
    TaskFailed,
    #[serde(rename = "step.started")]
    StepStarted,
    #[serde(rename = "step.completed")]
    StepCompleted,
    #[serde(rename = "step.failed")]
    StepFailed,
    #[serde(rename = "policy.permit")]
    PolicyPermit,
    #[serde(rename = "policy.deny")]
    PolicyDeny,
    #[serde(rename = "component.start")]
    ComponentStart,
    #[serde(rename = "component.stop")]
    ComponentStop,
    #[serde(rename = "step.awaiting_approval")]
    StepAwaitingApproval,
    #[serde(rename = "step.approved")]
    StepApproved,
    #[serde(rename = "step.rejected")]
    StepRejected,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct AuditEvent {
    pub task_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub step_id: String,
    pub event_type: EventType,
    pub timestamp: DateTime<Utc>,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub policy_source: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub policy_target: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub policy_capability: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub policy_deny_code: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub policy_deny_msg: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub component_id: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub component_ref: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub message: String,
}
