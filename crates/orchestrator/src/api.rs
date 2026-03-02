use crate::registry::AgentMeta;
use crate::scheduler::Orchestrator;
use axum::extract::{Multipart, Path, State};
use axum::http::StatusCode;
use axum::response::Json;
use chrono::Utc;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use tracing::info;
use wasm_af_taskstate::*;

pub type AppState = Arc<Orchestrator>;

#[derive(Deserialize)]
pub struct SubmitTaskRequest {
    #[serde(rename = "type")]
    pub task_type: String,
    #[serde(default)]
    pub query: String,
    #[serde(default)]
    pub context: HashMap<String, String>,
}

#[derive(Serialize)]
pub struct SubmitTaskResponse {
    pub task_id: String,
}

pub async fn handle_submit_task(
    State(orch): State<AppState>,
    Json(req): Json<SubmitTaskRequest>,
) -> Result<Json<SubmitTaskResponse>, (StatusCode, String)> {
    let task_id = uuid::Uuid::new_v4().to_string();

    // Evaluate submit policy
    {
        let mut policy = orch.policy.lock().unwrap();
        let input = serde_json::json!({
            "task_type": req.task_type,
            "query": req.query,
            "context": req.context,
        });
        match policy.evaluate_submit(input) {
            Ok(result) if !result.permitted => {
                let msg = result
                    .deny_message
                    .unwrap_or_else(|| "submission denied by policy".to_string());
                return Err((StatusCode::FORBIDDEN, msg));
            }
            Err(e) => {
                return Err((StatusCode::INTERNAL_SERVER_ERROR, format!("policy error: {e}")));
            }
            _ => {}
        }
    }

    // Build plan (simplified — uses generic builder)
    let plan = build_plan(&req, &task_id);

    let mut context = req.context.clone();
    context.insert("type".to_string(), req.task_type.clone());
    context.insert("query".to_string(), req.query.clone());

    let mut state = TaskState {
        task_id: task_id.clone(),
        status: Status::Pending,
        plan,
        current_step: 0,
        results: HashMap::new(),
        context,
        created_at: Utc::now(),
        updated_at: Utc::now(),
        error: String::new(),
    };

    orch.store
        .put(&mut state)
        .await
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("store: {e}")))?;

    orch.store
        .append_audit(&mut AuditEvent {
            task_id: task_id.clone(),
            event_type: EventType::TaskCreated,
            timestamp: Utc::now(),
            ..Default::default()
        })
        .await
        .ok();

    let orch_clone = orch.clone();
    let tid = task_id.clone();
    tokio::spawn(async move {
        orch_clone.run_task(tid).await;
    });

    Ok(Json(SubmitTaskResponse { task_id }))
}

pub async fn handle_get_task(
    State(orch): State<AppState>,
    Path(task_id): Path<String>,
) -> Result<Json<TaskState>, (StatusCode, String)> {
    match orch.store.get(&task_id).await {
        Ok(state) => Ok(Json(state)),
        Err(StoreError::NotFound(_)) => Err((StatusCode::NOT_FOUND, "task not found".to_string())),
        Err(e) => Err((StatusCode::INTERNAL_SERVER_ERROR, format!("{e}"))),
    }
}

#[derive(Serialize)]
pub struct ApprovalInfo {
    pub step_id: String,
    pub agent_type: String,
    pub reason: String,
}

pub async fn handle_list_approvals(
    State(orch): State<AppState>,
    Path(task_id): Path<String>,
) -> Result<Json<Vec<ApprovalInfo>>, (StatusCode, String)> {
    let state = orch
        .store
        .get(&task_id)
        .await
        .map_err(|e| (StatusCode::NOT_FOUND, format!("{e}")))?;

    let approvals: Vec<ApprovalInfo> = state
        .plan
        .iter()
        .filter(|s| s.status == StepStatus::AwaitingApproval)
        .map(|s| ApprovalInfo {
            step_id: s.id.clone(),
            agent_type: s.agent_type.clone(),
            reason: s.approval_reason.clone(),
        })
        .collect();

    Ok(Json(approvals))
}

#[derive(Deserialize)]
pub struct ApproveRequest {
    pub approved_by: String,
}

pub async fn handle_approve_step(
    State(orch): State<AppState>,
    Path((task_id, step_id)): Path<(String, String)>,
    Json(req): Json<ApproveRequest>,
) -> Result<StatusCode, (StatusCode, String)> {
    let now = Utc::now();
    let approved_by = req.approved_by.clone();
    let sid = step_id.clone();
    orch.store
        .update(&task_id, |s| {
            if let Some(idx) = s.plan.iter().position(|st| st.id == sid) {
                s.plan[idx].status = StepStatus::Pending;
                s.plan[idx].approved_by = approved_by.clone();
                s.plan[idx].approved_at = Some(now);
                Ok(())
            } else {
                Err(format!("step {sid} not found"))
            }
        })
        .await
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("{e}")))?;

    orch.store
        .append_audit(&mut AuditEvent {
            task_id: task_id.clone(),
            step_id: step_id.clone(),
            event_type: EventType::StepApproved,
            message: format!("approved by {}", req.approved_by),
            timestamp: Utc::now(),
            ..Default::default()
        })
        .await
        .ok();

    let orch_clone = orch.clone();
    let tid = task_id.clone();
    tokio::spawn(async move {
        orch_clone.run_task(tid).await;
    });

    Ok(StatusCode::OK)
}

#[derive(Deserialize)]
pub struct RejectRequest {
    pub rejected_by: String,
    #[serde(default)]
    pub reason: String,
}

pub async fn handle_reject_step(
    State(orch): State<AppState>,
    Path((task_id, step_id)): Path<(String, String)>,
    Json(req): Json<RejectRequest>,
) -> Result<StatusCode, (StatusCode, String)> {
    let reason = req.reason.clone();
    let sid = step_id.clone();
    orch.store
        .update(&task_id, |s| {
            if let Some(idx) = s.plan.iter().position(|st| st.id == sid) {
                s.plan[idx].status = StepStatus::Denied;
                s.plan[idx].error = format!("rejected by {}: {}", req.rejected_by, reason);
                Ok(())
            } else {
                Err(format!("step {sid} not found"))
            }
        })
        .await
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("{e}")))?;

    orch.store
        .append_audit(&mut AuditEvent {
            task_id: task_id.clone(),
            step_id: step_id.clone(),
            event_type: EventType::StepRejected,
            message: format!("rejected by {}", req.rejected_by),
            timestamp: Utc::now(),
            ..Default::default()
        })
        .await
        .ok();

    let orch_clone = orch.clone();
    let tid = task_id.clone();
    tokio::spawn(async move {
        orch_clone.run_task(tid).await;
    });

    Ok(StatusCode::OK)
}

#[derive(Serialize)]
pub struct AgentInfo {
    pub name: String,
    pub wasm_name: String,
    pub capability: String,
    pub context_key: String,
    pub host_functions: Vec<String>,
    pub external: bool,
}

pub async fn handle_list_agents(
    State(orch): State<AppState>,
) -> Json<Vec<AgentInfo>> {
    let agents = orch.registry.list();
    let list: Vec<AgentInfo> = agents
        .into_iter()
        .map(|(name, meta)| AgentInfo {
            name,
            wasm_name: meta.wasm_name,
            capability: meta.capability,
            context_key: meta.context_key,
            host_functions: meta.host_functions,
            external: meta.external,
        })
        .collect();
    Json(list)
}

pub async fn handle_register_agent(
    State(orch): State<AppState>,
    mut multipart: Multipart,
) -> Result<(StatusCode, String), (StatusCode, String)> {
    let mut meta_json: Option<String> = None;
    let mut wasm_bytes: Option<Vec<u8>> = None;

    while let Some(field) = multipart
        .next_field()
        .await
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("multipart: {e}")))?
    {
        let name = field.name().unwrap_or("").to_string();
        match name.as_str() {
            "meta" => {
                meta_json = Some(
                    field
                        .text()
                        .await
                        .map_err(|e| (StatusCode::BAD_REQUEST, format!("meta: {e}")))?,
                );
            }
            "wasm" => {
                wasm_bytes = Some(
                    field
                        .bytes()
                        .await
                        .map_err(|e| (StatusCode::BAD_REQUEST, format!("wasm: {e}")))?
                        .to_vec(),
                );
            }
            _ => {}
        }
    }

    let meta_str = meta_json.ok_or((StatusCode::BAD_REQUEST, "missing 'meta' field".to_string()))?;
    let wasm = wasm_bytes.ok_or((StatusCode::BAD_REQUEST, "missing 'wasm' field".to_string()))?;

    #[derive(Deserialize)]
    struct UploadMeta {
        name: String,
        #[serde(default)]
        context_key: String,
    }

    let upload: UploadMeta = serde_json::from_str(&meta_str)
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("meta json: {e}")))?;

    if upload.name.is_empty()
        || !upload
            .name
            .chars()
            .all(|c| c.is_alphanumeric() || c == '_' || c == '-')
    {
        return Err((StatusCode::BAD_REQUEST, "invalid agent name".to_string()));
    }

    if orch.registry.is_platform(&upload.name) {
        return Err((
            StatusCode::CONFLICT,
            "cannot overwrite platform agent".to_string(),
        ));
    }

    if wasm.len() > 10 * 1024 * 1024 {
        return Err((StatusCode::BAD_REQUEST, "wasm too large (max 10 MiB)".to_string()));
    }

    orch.engine
        .validate_wasm(&wasm)
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("validation failed: {e}")))?;

    let wasm_dir = orch.engine.wasm_dir();
    let external_path = wasm_dir.join("external");
    std::fs::create_dir_all(&external_path)
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("mkdir: {e}")))?;
    std::fs::write(
        external_path.join(format!("{}.wasm", upload.name)),
        &wasm,
    )
    .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("write: {e}")))?;

    let context_key = if upload.context_key.is_empty() {
        format!("{}_result", upload.name)
    } else {
        upload.context_key
    };

    orch.registry
        .register(
            &upload.name,
            AgentMeta {
                wasm_name: upload.name.clone(),
                capability: "untrusted".to_string(),
                context_key,
                host_functions: Vec::new(),
                payload_fields: HashMap::new(),
                enrichments: Vec::new(),
                splice: false,
                external: true,
            },
        )
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("{e}")))?;

    info!(name = %upload.name, "external agent registered");
    Ok((StatusCode::CREATED, format!("agent {} registered", upload.name)))
}

pub async fn handle_remove_agent(
    State(orch): State<AppState>,
    Path(name): Path<String>,
) -> Result<StatusCode, (StatusCode, String)> {
    if orch.registry.is_platform(&name) {
        return Err((
            StatusCode::FORBIDDEN,
            "cannot remove platform agent".to_string(),
        ));
    }
    orch.registry.remove(&name);
    Ok(StatusCode::NO_CONTENT)
}

pub async fn handle_healthz() -> StatusCode {
    StatusCode::OK
}

fn build_plan(req: &SubmitTaskRequest, task_id: &str) -> Vec<Step> {
    match req.task_type.as_str() {
        "chat" | "skill-demo" => build_chat_plan(req, task_id),
        "email-reply" => build_email_reply_plan(req, task_id),
        "reply-all" => build_reply_all_plan(req, task_id),
        "generic" => build_generic_plan(req, task_id),
        _ => build_generic_plan(req, task_id),
    }
}

fn make_step(task_id: &str, idx: usize, agent_type: &str, deps: &[usize], params: HashMap<String, String>) -> Step {
    let id = format!("{task_id}-step-{idx}");
    let depends_on: Vec<String> = deps.iter().map(|d| format!("{task_id}-step-{d}")).collect();
    Step {
        id: id.clone(),
        agent_type: agent_type.to_string(),
        input_key: format!("{id}.input"),
        output_key: format!("{id}.output"),
        status: StepStatus::Pending,
        depends_on,
        params,
        ..Default::default()
    }
}

/// Chat plan: memory(get) -> router -> responder -> memory(append)
/// The router has splice=true, so it can inject skill steps dynamically.
fn build_chat_plan(req: &SubmitTaskRequest, task_id: &str) -> Vec<Step> {
    let msg = req.query.clone();
    vec![
        make_step(task_id, 0, "memory", &[], HashMap::from([
            ("op".to_string(), "get".to_string()),
            ("key".to_string(), "conversation".to_string()),
        ])),
        make_step(task_id, 1, "router", &[0], HashMap::from([
            ("message".to_string(), msg.clone()),
        ])),
        make_step(task_id, 2, "responder", &[1], HashMap::from([
            ("message".to_string(), msg.clone()),
        ])),
        make_step(task_id, 3, "memory", &[2], HashMap::from([
            ("op".to_string(), "append".to_string()),
            ("key".to_string(), "conversation".to_string()),
        ])),
    ]
}

/// Email reply: email-read -> responder -> email-send
fn build_email_reply_plan(req: &SubmitTaskRequest, task_id: &str) -> Vec<Step> {
    let mut responder_params: HashMap<String, String> = HashMap::new();
    if let Some(msg) = req.context.get("message") {
        responder_params.insert("message".to_string(), msg.clone());
    }
    if let Some(idx) = req.context.get("reply_to_index") {
        responder_params.insert("reply_to_index".to_string(), idx.clone());
    }

    let mut send_params: HashMap<String, String> = HashMap::new();
    if let Some(to) = req.context.get("reply_to") {
        send_params.insert("to".to_string(), to.clone());
    }
    if let Some(subj) = req.context.get("reply_subject") {
        send_params.insert("subject".to_string(), subj.clone());
    }
    if let Some(body) = req.context.get("reply_body") {
        send_params.insert("body".to_string(), body.clone());
    }

    vec![
        make_step(task_id, 0, "email-read", &[], HashMap::from([
            ("folder".to_string(), "inbox".to_string()),
        ])),
        make_step(task_id, 1, "responder", &[0], responder_params),
        make_step(task_id, 2, "email-send", &[1], send_params),
    ]
}

/// Reply-all: email-read -> parallel responder branches -> parallel email-send branches
fn build_reply_all_plan(req: &SubmitTaskRequest, task_id: &str) -> Vec<Step> {
    let count: usize = req.context.get("email_count")
        .and_then(|c| c.parse().ok())
        .unwrap_or(2);

    let mut steps = vec![
        make_step(task_id, 0, "email-read", &[], HashMap::from([
            ("folder".to_string(), "inbox".to_string()),
        ])),
    ];

    for i in 0..count {
        let resp_idx = 1 + i * 2;
        let send_idx = 2 + i * 2;

        let mut resp_params: HashMap<String, String> = HashMap::new();
        resp_params.insert("reply_to_index".to_string(), i.to_string());
        if let Some(msg) = req.context.get("message") {
            resp_params.insert("message".to_string(), msg.clone());
        }

        let mut send_params: HashMap<String, String> = HashMap::new();
        if let Some(to) = req.context.get(&format!("reply_to_{i}")) {
            send_params.insert("to".to_string(), to.clone());
        }
        if let Some(subj) = req.context.get(&format!("reply_subject_{i}")) {
            send_params.insert("subject".to_string(), subj.clone());
        }
        if let Some(body) = req.context.get(&format!("reply_body_{i}")) {
            send_params.insert("body".to_string(), body.clone());
        }

        steps.push(make_step(task_id, resp_idx, "responder", &[0], resp_params));
        steps.push(make_step(task_id, send_idx, "email-send", &[resp_idx], send_params));
    }

    steps
}

fn build_generic_plan(req: &SubmitTaskRequest, task_id: &str) -> Vec<Step> {
    if let Some(steps_json) = req.context.get("steps") {
        #[derive(Deserialize)]
        struct StepSpec {
            agent_type: String,
            #[serde(default)]
            params: HashMap<String, String>,
            #[serde(default)]
            depends_on: Vec<String>,
        }
        if let Ok(specs) = serde_json::from_str::<Vec<StepSpec>>(steps_json) {
            return specs
                .into_iter()
                .enumerate()
                .map(|(i, spec)| {
                    let id = format!("{task_id}-step-{i}");
                    Step {
                        id: id.clone(),
                        agent_type: spec.agent_type,
                        input_key: format!("{id}.input"),
                        output_key: format!("{id}.output"),
                        status: StepStatus::Pending,
                        depends_on: spec.depends_on,
                        params: spec.params,
                        ..Default::default()
                    }
                })
                .collect();
        }
    }

    let step_id = format!("{task_id}-step-0");
    vec![Step {
        id: step_id.clone(),
        agent_type: req.task_type.clone(),
        input_key: format!("{step_id}.input"),
        output_key: format!("{step_id}.output"),
        status: StepStatus::Pending,
        params: req
            .context
            .iter()
            .map(|(k, v)| (k.clone(), v.clone()))
            .collect(),
        ..Default::default()
    }]
}
