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
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    pub taint: HashMap<String, Vec<String>>,
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
    #[serde(rename = "taint.declassified")]
    TaintDeclassified,
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

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::TimeZone;

    fn sample_task_state() -> TaskState {
        let ts = Utc.with_ymd_and_hms(2025, 6, 1, 12, 0, 0).unwrap();
        TaskState {
            task_id: "t1".into(),
            status: Status::Running,
            plan: vec![Step {
                id: "t1-step-0".into(),
                agent_type: "shell".into(),
                input_key: "t1-step-0.input".into(),
                output_key: "t1-step-0.output".into(),
                status: StepStatus::Completed,
                depends_on: vec!["t1-init".into()],
                params: HashMap::from([("command".into(), "ls".into())]),
                ..Default::default()
            }],
            current_step: 1,
            results: HashMap::from([("t1-step-0".into(), "ok".into())]),
            context: HashMap::from([("message".into(), "hello".into())]),
            created_at: ts,
            updated_at: ts,
            error: String::new(),
            taint: HashMap::new(),
        }
    }

    #[test]
    fn task_state_round_trip() {
        let state = sample_task_state();
        let json = serde_json::to_string(&state).unwrap();
        let back: TaskState = serde_json::from_str(&json).unwrap();
        assert_eq!(back.task_id, "t1");
        assert_eq!(back.status, Status::Running);
        assert_eq!(back.plan.len(), 1);
        assert_eq!(back.plan[0].agent_type, "shell");
        assert_eq!(back.current_step, 1);
    }

    #[test]
    fn status_serializes_to_snake_case() {
        assert_eq!(
            serde_json::to_string(&Status::Pending).unwrap(),
            "\"pending\""
        );
        assert_eq!(
            serde_json::to_string(&Status::Running).unwrap(),
            "\"running\""
        );
        assert_eq!(
            serde_json::to_string(&Status::Completed).unwrap(),
            "\"completed\""
        );
        assert_eq!(
            serde_json::to_string(&Status::Failed).unwrap(),
            "\"failed\""
        );
        assert_eq!(
            serde_json::to_string(&Status::AwaitingApproval).unwrap(),
            "\"awaiting_approval\""
        );
    }

    #[test]
    fn status_deserializes_from_snake_case() {
        let s: Status = serde_json::from_str("\"awaiting_approval\"").unwrap();
        assert_eq!(s, Status::AwaitingApproval);
    }

    #[test]
    fn step_status_default_is_pending() {
        assert_eq!(StepStatus::default(), StepStatus::Pending);
    }

    #[test]
    fn step_status_round_trip() {
        for (variant, expected) in [
            (StepStatus::Pending, "\"pending\""),
            (StepStatus::Running, "\"running\""),
            (StepStatus::Completed, "\"completed\""),
            (StepStatus::Failed, "\"failed\""),
            (StepStatus::Denied, "\"denied\""),
            (StepStatus::AwaitingApproval, "\"awaiting_approval\""),
        ] {
            let json = serde_json::to_string(&variant).unwrap();
            assert_eq!(json, expected);
            let back: StepStatus = serde_json::from_str(&json).unwrap();
            assert_eq!(back, variant);
        }
    }

    #[test]
    fn event_type_serializes_to_dotted_names() {
        let cases: Vec<(EventType, &str)> = vec![
            (EventType::TaskCreated, "\"task.created\""),
            (EventType::TaskCompleted, "\"task.completed\""),
            (EventType::TaskFailed, "\"task.failed\""),
            (EventType::StepStarted, "\"step.started\""),
            (EventType::StepCompleted, "\"step.completed\""),
            (EventType::StepFailed, "\"step.failed\""),
            (EventType::PolicyPermit, "\"policy.permit\""),
            (EventType::PolicyDeny, "\"policy.deny\""),
            (EventType::ComponentStart, "\"component.start\""),
            (EventType::ComponentStop, "\"component.stop\""),
            (
                EventType::StepAwaitingApproval,
                "\"step.awaiting_approval\"",
            ),
            (EventType::StepApproved, "\"step.approved\""),
            (EventType::StepRejected, "\"step.rejected\""),
            (EventType::TaintDeclassified, "\"taint.declassified\""),
        ];
        for (variant, expected) in cases {
            let json = serde_json::to_string(&variant).unwrap();
            assert_eq!(json, expected, "mismatch for {variant:?}");
            let back: EventType = serde_json::from_str(&json).unwrap();
            assert_eq!(back, variant);
        }
    }

    #[test]
    fn event_type_default_is_task_created() {
        assert_eq!(EventType::default(), EventType::TaskCreated);
    }

    #[test]
    fn step_skip_serializing_empty_fields() {
        let step = Step::default();
        let json = serde_json::to_string(&step).unwrap();
        let v: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert!(v.get("error").is_none(), "empty error should be omitted");
        assert!(
            v.get("depends_on").is_none(),
            "empty depends_on should be omitted"
        );
        assert!(v.get("params").is_none(), "empty params should be omitted");
        assert!(
            v.get("started_at").is_none(),
            "None started_at should be omitted"
        );
        assert!(
            v.get("approval_reason").is_none(),
            "empty approval_reason should be omitted"
        );
        assert!(
            v.get("approved_by").is_none(),
            "empty approved_by should be omitted"
        );
        assert!(
            v.get("approved_at").is_none(),
            "None approved_at should be omitted"
        );
    }

    #[test]
    fn step_populated_fields_present() {
        let step = Step {
            id: "s1".into(),
            agent_type: "shell".into(),
            error: "boom".into(),
            depends_on: vec!["s0".into()],
            params: HashMap::from([("k".into(), "v".into())]),
            approval_reason: "sensitive".into(),
            ..Default::default()
        };
        let json = serde_json::to_string(&step).unwrap();
        let v: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(v["error"], "boom");
        assert_eq!(v["depends_on"], serde_json::json!(["s0"]));
        assert_eq!(v["params"]["k"], "v");
        assert_eq!(v["approval_reason"], "sensitive");
    }

    #[test]
    fn audit_event_round_trip() {
        let ts = Utc.with_ymd_and_hms(2025, 6, 1, 12, 0, 0).unwrap();
        let event = AuditEvent {
            task_id: "t1".into(),
            step_id: "s1".into(),
            event_type: EventType::PolicyDeny,
            timestamp: ts,
            policy_deny_code: "FORBIDDEN".into(),
            policy_deny_msg: "not allowed".into(),
            ..Default::default()
        };
        let json = serde_json::to_string(&event).unwrap();
        let back: AuditEvent = serde_json::from_str(&json).unwrap();
        assert_eq!(back.task_id, "t1");
        assert_eq!(back.event_type, EventType::PolicyDeny);
        assert_eq!(back.policy_deny_code, "FORBIDDEN");
    }

    #[test]
    fn audit_event_skips_empty_optional_fields() {
        let event = AuditEvent {
            task_id: "t1".into(),
            event_type: EventType::TaskCreated,
            timestamp: Utc::now(),
            ..Default::default()
        };
        let json = serde_json::to_string(&event).unwrap();
        let v: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert!(v.get("step_id").is_none());
        assert!(v.get("policy_source").is_none());
        assert!(v.get("component_id").is_none());
        assert!(v.get("message").is_none());
    }

    #[test]
    fn task_state_empty_error_omitted() {
        let state = sample_task_state();
        let json = serde_json::to_string(&state).unwrap();
        let v: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert!(
            v.get("error").is_none(),
            "empty error should be omitted from TaskState"
        );
    }

    #[test]
    fn task_state_deserializes_without_optional_fields() {
        let json = r#"{
            "task_id": "t1",
            "status": "pending",
            "plan": [],
            "created_at": "2025-06-01T12:00:00Z",
            "updated_at": "2025-06-01T12:00:00Z"
        }"#;
        let state: TaskState = serde_json::from_str(json).unwrap();
        assert_eq!(state.current_step, 0);
        assert!(state.results.is_empty());
        assert!(state.context.is_empty());
        assert!(state.error.is_empty());
        assert!(state.taint.is_empty());
    }

    #[test]
    fn task_state_taint_round_trip() {
        let ts = Utc.with_ymd_and_hms(2025, 6, 1, 12, 0, 0).unwrap();
        let mut taint = HashMap::new();
        taint.insert(
            "t1-step-0.output".to_string(),
            vec!["web".to_string(), "external".to_string()],
        );
        let state = TaskState {
            task_id: "t1".into(),
            status: Status::Running,
            plan: vec![],
            current_step: 0,
            results: HashMap::new(),
            context: HashMap::new(),
            created_at: ts,
            updated_at: ts,
            error: String::new(),
            taint,
        };
        let json = serde_json::to_string(&state).unwrap();
        let back: TaskState = serde_json::from_str(&json).unwrap();
        assert_eq!(back.taint.len(), 1);
        let labels = back.taint.get("t1-step-0.output").unwrap();
        assert!(labels.contains(&"web".to_string()));
        assert!(labels.contains(&"external".to_string()));
    }

    #[test]
    fn task_state_empty_taint_omitted() {
        let state = sample_task_state();
        let json = serde_json::to_string(&state).unwrap();
        let v: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert!(
            v.get("taint").is_none(),
            "empty taint map should be omitted from serialized JSON"
        );
    }

    #[test]
    fn taint_declassified_event_type() {
        let json = serde_json::to_string(&EventType::TaintDeclassified).unwrap();
        assert_eq!(json, "\"taint.declassified\"");
        let back: EventType = serde_json::from_str(&json).unwrap();
        assert_eq!(back, EventType::TaintDeclassified);
    }
}
