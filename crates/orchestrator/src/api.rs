use crate::registry::AgentMeta;
use crate::scheduler::Orchestrator;
use axum::extract::{Multipart, Path, State};
use axum::http::StatusCode;
use axum::response::Json;
use chrono::Utc;
use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;
use tracing::{info, warn};
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
) -> Result<(StatusCode, Json<SubmitTaskResponse>), (StatusCode, String)> {
    let task_id = uuid::Uuid::new_v4().to_string();

    // Evaluate submit policy
    {
        let mut policy = orch.policy.lock().map_err(|e| {
            (
                StatusCode::INTERNAL_SERVER_ERROR,
                format!("policy lock poisoned: {e}"),
            )
        })?;
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
                return Err((
                    StatusCode::INTERNAL_SERVER_ERROR,
                    format!("policy error: {e}"),
                ));
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
        taint: HashMap::new(),
    };

    orch.store
        .put(&mut state)
        .await
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("store: {e}")))?;

    if let Err(e) = orch
        .store
        .append_audit(&mut AuditEvent {
            task_id: task_id.clone(),
            event_type: EventType::TaskCreated,
            timestamp: Utc::now(),
            ..Default::default()
        })
        .await
    {
        warn!(task_id = %task_id, err = %e, "audit: task-created write failed");
    }

    let orch_clone = orch.clone();
    let tid = task_id.clone();
    tokio::spawn(async move {
        orch_clone.run_task(tid).await;
    });

    Ok((StatusCode::ACCEPTED, Json(SubmitTaskResponse { task_id })))
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

fn apply_approve(
    state: &mut TaskState,
    step_id: &str,
    approved_by: &str,
    now: chrono::DateTime<Utc>,
) -> Result<(), String> {
    if let Some(idx) = state.plan.iter().position(|st| st.id == step_id) {
        state.plan[idx].status = StepStatus::Pending;
        state.plan[idx].approved_by = approved_by.to_string();
        state.plan[idx].approved_at = Some(now);
        Ok(())
    } else {
        Err(format!("step {step_id} not found"))
    }
}

fn apply_reject(
    state: &mut TaskState,
    step_id: &str,
    rejected_by: &str,
    reason: &str,
) -> Result<(), String> {
    if let Some(idx) = state.plan.iter().position(|st| st.id == step_id) {
        state.plan[idx].status = StepStatus::Denied;
        state.plan[idx].error = format!("rejected by {rejected_by}: {reason}");
        Ok(())
    } else {
        Err(format!("step {step_id} not found"))
    }
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
        .update(&task_id, |s| apply_approve(s, &sid, &approved_by, now))
        .await
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("{e}")))?;

    if let Err(e) = orch
        .store
        .append_audit(&mut AuditEvent {
            task_id: task_id.clone(),
            step_id: step_id.clone(),
            event_type: EventType::StepApproved,
            message: format!("approved by {}", req.approved_by),
            timestamp: Utc::now(),
            ..Default::default()
        })
        .await
    {
        warn!(task_id = %task_id, step_id = %step_id, err = %e, "audit: step-approved write failed");
    }

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
    let sid = step_id.clone();
    let rejected_by = req.rejected_by.clone();
    let reason = req.reason.clone();
    orch.store
        .update(&task_id, |s| apply_reject(s, &sid, &rejected_by, &reason))
        .await
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("{e}")))?;

    if let Err(e) = orch
        .store
        .append_audit(&mut AuditEvent {
            task_id: task_id.clone(),
            step_id: step_id.clone(),
            event_type: EventType::StepRejected,
            message: format!("rejected by {}", req.rejected_by),
            timestamp: Utc::now(),
            ..Default::default()
        })
        .await
    {
        warn!(task_id = %task_id, step_id = %step_id, err = %e, "audit: step-rejected write failed");
    }

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

pub async fn handle_list_agents(State(orch): State<AppState>) -> Json<Vec<AgentInfo>> {
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

    let meta_str =
        meta_json.ok_or((StatusCode::BAD_REQUEST, "missing 'meta' field".to_string()))?;
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

    if wasm.len() > 50 * 1024 * 1024 {
        return Err((
            StatusCode::BAD_REQUEST,
            "wasm too large (max 50 MiB)".to_string(),
        ));
    }

    orch.engine
        .validate_byoa_wasm(&wasm)
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("validation failed: {e}")))?;

    let wasm_dir = orch.engine.wasm_dir();
    let external_path = wasm_dir.join("external");
    std::fs::create_dir_all(&external_path)
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("mkdir: {e}")))?;
    let final_path = external_path.join(format!("{}.wasm", upload.name));
    let tmp_path = external_path.join(format!(".{}.wasm.tmp", upload.name));
    std::fs::write(&tmp_path, &wasm)
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("write tmp: {e}")))?;
    std::fs::rename(&tmp_path, &final_path).map_err(|e| {
        let _ = std::fs::remove_file(&tmp_path);
        (StatusCode::INTERNAL_SERVER_ERROR, format!("rename: {e}"))
    })?;

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
                output_taint: vec!["untrusted".to_string()],
                declassifies: Vec::new(),
            },
        )
        .map_err(|e| (StatusCode::BAD_REQUEST, format!("{e}")))?;

    // Evict any cached compiled component so the next invocation loads the new file.
    orch.engine.evict_component(&final_path);

    info!(name = %upload.name, "external agent registered");
    Ok((
        StatusCode::CREATED,
        format!("agent {} registered", upload.name),
    ))
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

    // Evict the cached component so it can't be invoked after removal.
    if let Ok(path) = orch.engine.wasm_path(&name) {
        orch.engine.evict_component(&path);
    }

    Ok(StatusCode::NO_CONTENT)
}

pub async fn handle_healthz() -> StatusCode {
    StatusCode::OK
}

// ── Synchronous chat endpoint (/message) ────────────────────────────────────

const INITIAL_POLL_INTERVAL: Duration = Duration::from_millis(250);
const MAX_POLL_INTERVAL: Duration = Duration::from_secs(2);
const MAX_POLL_DURATION: Duration = Duration::from_secs(30);

#[derive(Deserialize)]
pub struct MessageRequest {
    pub message: String,
    #[serde(default)]
    pub user: String,
}

#[derive(Serialize)]
pub struct MessageResponse {
    pub response: String,
    pub task_id: String,
}

pub async fn handle_message(
    State(orch): State<AppState>,
    Json(req): Json<MessageRequest>,
) -> Result<Json<MessageResponse>, (StatusCode, String)> {
    if req.message.is_empty() {
        return Err((StatusCode::BAD_REQUEST, "message is required".to_string()));
    }
    let user = if req.user.is_empty() {
        "anonymous".to_string()
    } else {
        req.user
    };

    info!(user = %user, message_len = req.message.len(), "chat message received");

    let submit_req = SubmitTaskRequest {
        task_type: "chat".to_string(),
        query: req.message.clone(),
        context: HashMap::from([
            ("user".to_string(), user),
            ("message".to_string(), req.message),
        ]),
    };

    let (_, Json(submit_resp)) = handle_submit_task(State(orch.clone()), Json(submit_req)).await?;
    let task_id = submit_resp.task_id;

    let response = poll_for_response(&orch, &task_id).await?;

    Ok(Json(MessageResponse { response, task_id }))
}

async fn poll_for_response(orch: &AppState, task_id: &str) -> Result<String, (StatusCode, String)> {
    let deadline = tokio::time::Instant::now() + MAX_POLL_DURATION;
    let mut interval = INITIAL_POLL_INTERVAL;

    loop {
        tokio::time::sleep(interval).await;

        if tokio::time::Instant::now() >= deadline {
            return Err((
                StatusCode::GATEWAY_TIMEOUT,
                "task did not complete in time".to_string(),
            ));
        }

        let state = match orch.store.get(task_id).await {
            Ok(s) => s,
            Err(_) => {
                interval = backoff(interval);
                continue;
            }
        };

        match state.status {
            Status::Completed => return Ok(extract_response(&state)),
            Status::Failed => {
                return Err((
                    StatusCode::INTERNAL_SERVER_ERROR,
                    format!("task failed: {}", state.error),
                ));
            }
            _ => {
                interval = backoff(interval);
            }
        }
    }
}

fn backoff(current: Duration) -> Duration {
    let next = current * 2;
    if next > MAX_POLL_INTERVAL {
        MAX_POLL_INTERVAL
    } else {
        next
    }
}

fn extract_response(state: &TaskState) -> String {
    if let Some(rk) = state.context.get("result_key") {
        if let Some(payload) = state.results.get(rk) {
            return extract_payload_text(payload);
        }
    }

    for step in &state.plan {
        if step.agent_type != "responder" {
            continue;
        }
        if let Some(payload) = state.results.get(&step.output_key) {
            return extract_payload_text(payload);
        }
    }
    "(no response)".to_string()
}

fn extract_payload_text(payload: &str) -> String {
    #[derive(Deserialize)]
    struct PayloadResponse {
        #[serde(default)]
        response: String,
    }
    if let Ok(parsed) = serde_json::from_str::<PayloadResponse>(payload) {
        if !parsed.response.is_empty() {
            return parsed.response;
        }
    }
    payload.to_string()
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

fn make_step(
    task_id: &str,
    idx: usize,
    agent_type: &str,
    deps: &[usize],
    params: HashMap<String, String>,
) -> Step {
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
        make_step(
            task_id,
            0,
            "memory",
            &[],
            HashMap::from([
                ("op".to_string(), "get".to_string()),
                ("key".to_string(), "conversation".to_string()),
            ]),
        ),
        make_step(
            task_id,
            1,
            "router",
            &[0],
            HashMap::from([("message".to_string(), msg.clone())]),
        ),
        make_step(
            task_id,
            2,
            "responder",
            &[1],
            HashMap::from([("message".to_string(), msg.clone())]),
        ),
        make_step(
            task_id,
            3,
            "memory",
            &[2],
            HashMap::from([
                ("op".to_string(), "append".to_string()),
                ("key".to_string(), "conversation".to_string()),
            ]),
        ),
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
        make_step(
            task_id,
            0,
            "email-read",
            &[],
            HashMap::from([("folder".to_string(), "inbox".to_string())]),
        ),
        make_step(task_id, 1, "responder", &[0], responder_params),
        make_step(task_id, 2, "email-send", &[1], send_params),
    ]
}

/// Reply-all: email-read -> parallel responder branches -> parallel email-send branches
fn build_reply_all_plan(req: &SubmitTaskRequest, task_id: &str) -> Vec<Step> {
    let count: usize = req
        .context
        .get("email_count")
        .and_then(|c| c.parse().ok())
        .unwrap_or(2);

    let mut steps = vec![make_step(
        task_id,
        0,
        "email-read",
        &[],
        HashMap::from([("folder".to_string(), "inbox".to_string())]),
    )];

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
        steps.push(make_step(
            task_id,
            send_idx,
            "email-send",
            &[resp_idx],
            send_params,
        ));
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
                    let depends_on = spec
                        .depends_on
                        .iter()
                        .map(|dep| {
                            if dep.parse::<usize>().is_ok() {
                                format!("{task_id}-step-{dep}")
                            } else {
                                dep.clone()
                            }
                        })
                        .collect();
                    Step {
                        id: id.clone(),
                        agent_type: spec.agent_type,
                        input_key: format!("{id}.input"),
                        output_key: format!("{id}.output"),
                        status: StepStatus::Pending,
                        depends_on,
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

#[cfg(test)]
mod tests {
    use super::*;

    fn req(task_type: &str, query: &str, ctx: Vec<(&str, &str)>) -> SubmitTaskRequest {
        SubmitTaskRequest {
            task_type: task_type.into(),
            query: query.into(),
            context: ctx.into_iter().map(|(k, v)| (k.into(), v.into())).collect(),
        }
    }

    // ---- Chat plan ----

    #[test]
    fn chat_plan_has_four_steps() {
        let r = req("chat", "hello", vec![("message", "hello")]);
        let plan = build_plan(&r, "t1");
        assert_eq!(plan.len(), 4);
    }

    #[test]
    fn chat_plan_step_types() {
        let r = req("chat", "hello", vec![("message", "hello")]);
        let plan = build_plan(&r, "t1");
        let types: Vec<&str> = plan.iter().map(|s| s.agent_type.as_str()).collect();
        assert_eq!(types, vec!["memory", "router", "responder", "memory"]);
    }

    #[test]
    fn chat_plan_dependency_chain() {
        let r = req("chat", "hello", vec![]);
        let plan = build_plan(&r, "t1");
        assert!(plan[0].depends_on.is_empty());
        assert_eq!(plan[1].depends_on, vec!["t1-step-0"]);
        assert_eq!(plan[2].depends_on, vec!["t1-step-1"]);
        assert_eq!(plan[3].depends_on, vec!["t1-step-2"]);
    }

    // ---- Reply-all plan ----

    #[test]
    fn reply_all_plan_default_two_emails() {
        let r = req("reply-all", "reply all", vec![("message", "reply")]);
        let plan = build_plan(&r, "t1");
        // 1 email-read + 2*(responder + email-send) = 5
        assert_eq!(plan.len(), 5);
        assert_eq!(plan[0].agent_type, "email-read");
    }

    #[test]
    fn reply_all_plan_parallel_branches() {
        let r = req("reply-all", "reply all", vec![("email_count", "3")]);
        let plan = build_plan(&r, "t1");
        // 1 + 3*2 = 7
        assert_eq!(plan.len(), 7);
        // All responders depend on email-read (step 0), not each other
        assert_eq!(plan[1].depends_on, vec!["t1-step-0"]);
        assert_eq!(plan[3].depends_on, vec!["t1-step-0"]);
        assert_eq!(plan[5].depends_on, vec!["t1-step-0"]);
    }

    #[test]
    fn reply_all_email_send_depends_on_its_responder() {
        let r = req("reply-all", "reply all", vec![("email_count", "2")]);
        let plan = build_plan(&r, "t1");
        // email-send[0] at index 2 depends on responder[0] at index 1
        assert_eq!(plan[2].agent_type, "email-send");
        assert_eq!(plan[2].depends_on, vec!["t1-step-1"]);
        // email-send[1] at index 4 depends on responder[1] at index 3
        assert_eq!(plan[4].agent_type, "email-send");
        assert_eq!(plan[4].depends_on, vec!["t1-step-3"]);
    }

    // ---- Generic plan ----

    #[test]
    fn generic_plan_with_steps_json() {
        let steps_json = r#"[
            {"agent_type": "web-search", "params": {"query": "test"}},
            {"agent_type": "summarizer", "depends_on": ["t1-step-0"]}
        ]"#;
        let r = req("generic", "", vec![("steps", steps_json)]);
        let plan = build_plan(&r, "t1");
        assert_eq!(plan.len(), 2);
        assert_eq!(plan[0].agent_type, "web-search");
        assert_eq!(plan[0].params.get("query").unwrap(), "test");
        assert_eq!(plan[1].depends_on, vec!["t1-step-0"]);
    }

    #[test]
    fn generic_plan_resolves_numeric_depends_on() {
        let steps_json = r#"[
            {"agent_type": "url-fetch", "params": {"url": "https://x.com"}},
            {"agent_type": "pii-redactor", "depends_on": ["0"]},
            {"agent_type": "responder", "depends_on": ["1"]}
        ]"#;
        let r = req("pii-pipeline", "", vec![("steps", steps_json)]);
        let plan = build_plan(&r, "t1");
        assert_eq!(plan.len(), 3);
        assert_eq!(plan[1].depends_on, vec!["t1-step-0"]);
        assert_eq!(plan[2].depends_on, vec!["t1-step-1"]);
    }

    #[test]
    fn generic_plan_preserves_full_step_ids_in_depends_on() {
        let steps_json = r#"[
            {"agent_type": "a"},
            {"agent_type": "b", "depends_on": ["t1-step-0"]}
        ]"#;
        let r = req("generic", "", vec![("steps", steps_json)]);
        let plan = build_plan(&r, "t1");
        assert_eq!(plan[1].depends_on, vec!["t1-step-0"]);
    }

    #[test]
    fn generic_plan_fallback_single_step() {
        let r = req("custom-task", "", vec![("key", "value")]);
        let plan = build_plan(&r, "t1");
        assert_eq!(plan.len(), 1);
        assert_eq!(plan[0].agent_type, "custom-task");
        assert_eq!(plan[0].params.get("key").unwrap(), "value");
    }

    // ---- Step structure ----

    #[test]
    fn step_keys_are_correct() {
        let r = req("chat", "hi", vec![]);
        let plan = build_plan(&r, "task-123");
        assert_eq!(plan[0].id, "task-123-step-0");
        assert_eq!(plan[0].input_key, "task-123-step-0.input");
        assert_eq!(plan[0].output_key, "task-123-step-0.output");
    }

    // ---- Approval / rejection logic ----

    fn task_with_step(step_id: &str, status: StepStatus) -> TaskState {
        TaskState {
            task_id: "t1".into(),
            status: Status::Running,
            plan: vec![Step {
                id: step_id.into(),
                agent_type: "email-send".into(),
                status,
                ..Default::default()
            }],
            current_step: 0,
            results: HashMap::new(),
            context: HashMap::new(),
            created_at: Utc::now(),
            updated_at: Utc::now(),
            error: String::new(),
            taint: HashMap::new(),
        }
    }

    #[test]
    fn approve_step_success() {
        let mut state = task_with_step("s1", StepStatus::AwaitingApproval);
        let now = Utc::now();
        let result = super::apply_approve(&mut state, "s1", "alice", now);
        assert!(result.is_ok());
        assert_eq!(state.plan[0].status, StepStatus::Pending);
        assert_eq!(state.plan[0].approved_by, "alice");
        assert!(state.plan[0].approved_at.is_some());
    }

    #[test]
    fn approve_step_not_found() {
        let mut state = task_with_step("s1", StepStatus::AwaitingApproval);
        let result = super::apply_approve(&mut state, "nonexistent", "alice", Utc::now());
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("not found"));
    }

    #[test]
    fn reject_step_success() {
        let mut state = task_with_step("s1", StepStatus::AwaitingApproval);
        let result = super::apply_reject(&mut state, "s1", "bob", "not safe");
        assert!(result.is_ok());
        assert_eq!(state.plan[0].status, StepStatus::Denied);
        assert!(state.plan[0].error.contains("rejected by bob"));
        assert!(state.plan[0].error.contains("not safe"));
    }

    #[test]
    fn reject_step_not_found() {
        let mut state = task_with_step("s1", StepStatus::AwaitingApproval);
        let result = super::apply_reject(&mut state, "nonexistent", "bob", "reason");
        assert!(result.is_err());
        assert!(result.unwrap_err().contains("not found"));
    }

    // ---- extract_payload_text ----

    #[test]
    fn extract_payload_text_valid_json() {
        let payload = r#"{"response":"hello world"}"#;
        assert_eq!(super::extract_payload_text(payload), "hello world");
    }

    #[test]
    fn extract_payload_text_empty_response_returns_raw() {
        let payload = r#"{"response":""}"#;
        assert_eq!(super::extract_payload_text(payload), payload);
    }

    #[test]
    fn extract_payload_text_plain_string() {
        assert_eq!(super::extract_payload_text("just text"), "just text");
    }

    #[test]
    fn extract_payload_text_invalid_json() {
        assert_eq!(super::extract_payload_text("{broken"), "{broken");
    }

    #[test]
    fn extract_payload_text_no_response_key() {
        let payload = r#"{"other":"data"}"#;
        assert_eq!(super::extract_payload_text(payload), payload);
    }

    // ---- extract_response ----

    #[test]
    fn extract_response_uses_result_key() {
        let state = TaskState {
            task_id: "t1".into(),
            status: Status::Completed,
            plan: vec![],
            current_step: 0,
            results: HashMap::from([(
                "t1-step-2.output".to_string(),
                r#"{"response":"from result_key"}"#.to_string(),
            )]),
            context: HashMap::from([("result_key".to_string(), "t1-step-2.output".to_string())]),
            created_at: Utc::now(),
            updated_at: Utc::now(),
            error: String::new(),
            taint: HashMap::new(),
        };
        assert_eq!(super::extract_response(&state), "from result_key");
    }

    #[test]
    fn extract_response_falls_back_to_responder() {
        let state = TaskState {
            task_id: "t1".into(),
            status: Status::Completed,
            plan: vec![
                Step {
                    id: "t1-step-0".into(),
                    agent_type: "memory".into(),
                    output_key: "t1-step-0.output".into(),
                    ..Default::default()
                },
                Step {
                    id: "t1-step-2".into(),
                    agent_type: "responder".into(),
                    output_key: "t1-step-2.output".into(),
                    ..Default::default()
                },
            ],
            current_step: 0,
            results: HashMap::from([(
                "t1-step-2.output".to_string(),
                r#"{"response":"from responder"}"#.to_string(),
            )]),
            context: HashMap::new(),
            created_at: Utc::now(),
            updated_at: Utc::now(),
            error: String::new(),
            taint: HashMap::new(),
        };
        assert_eq!(super::extract_response(&state), "from responder");
    }

    #[test]
    fn extract_response_no_match_returns_fallback() {
        let state = TaskState {
            task_id: "t1".into(),
            status: Status::Completed,
            plan: vec![Step {
                agent_type: "memory".into(),
                ..Default::default()
            }],
            current_step: 0,
            results: HashMap::new(),
            context: HashMap::new(),
            created_at: Utc::now(),
            updated_at: Utc::now(),
            error: String::new(),
            taint: HashMap::new(),
        };
        assert_eq!(super::extract_response(&state), "(no response)");
    }

    // ---- backoff ----

    #[test]
    fn backoff_doubles() {
        assert_eq!(
            super::backoff(Duration::from_millis(250)),
            Duration::from_millis(500)
        );
    }

    #[test]
    fn backoff_caps_at_max() {
        assert_eq!(
            super::backoff(Duration::from_secs(2)),
            super::MAX_POLL_INTERVAL
        );
        assert_eq!(
            super::backoff(Duration::from_secs(5)),
            super::MAX_POLL_INTERVAL
        );
    }
}
