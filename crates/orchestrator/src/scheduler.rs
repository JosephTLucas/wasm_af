use crate::engine::{sanitize_allowed_paths, KvPair, PluginOpts, TaskInput, WasmEngine};
use crate::host::{
    ConfigState, EmailState, ExecState, HostState, KvState, LlmState, SandboxState, StepMeta,
};
use crate::policy::OpaEvaluator;
use crate::registry::{build_payload, enrich_params, AgentRegistry};
use chrono::Utc;
use serde::Deserialize;
use std::collections::{HashMap, HashSet};
use std::sync::{Arc, Mutex};
use std::time::Duration;
use tracing::{error, info, warn};
use wasm_af_dag::Graph;
use wasm_af_taskstate::*;

pub struct Orchestrator {
    pub engine: Arc<WasmEngine>,
    pub store: Arc<Store>,
    pub policy: Arc<Mutex<OpaEvaluator>>,
    pub registry: Arc<AgentRegistry>,
    pub llm_state: LlmState,
    pub kv_state: KvState,
    pub exec_state: ExecState,
    pub sandbox_state: SandboxState,
    pub email_state: EmailState,

    pub plugin_timeout: Duration,
    pub plugin_max_mem_pages: u64,

    pub config_kv: async_nats::jetstream::kv::Store,
    pub approval_webhook_url: String,
    pub approval_timeout_sec: u64,

    pub running_tasks: Arc<tokio::sync::Mutex<HashSet<String>>>,
}

fn build_dag(plan: &[Step]) -> Result<Graph, wasm_af_dag::DagError> {
    let ids: Vec<String> = plan.iter().map(|s| s.id.clone()).collect();
    let mut deps = HashMap::new();
    for s in plan {
        if !s.depends_on.is_empty() {
            deps.insert(s.id.clone(), s.depends_on.clone());
        }
    }
    Graph::new(&ids, &deps)
}

fn step_index(plan: &[Step], id: &str) -> Option<usize> {
    plan.iter().position(|s| s.id == id)
}

/// Partition plan steps into completed and non-dispatchable (failed/denied/awaiting) sets.
fn plan_status_sets(plan: &[Step]) -> (HashSet<String>, HashSet<String>) {
    let mut completed = HashSet::new();
    let mut non_dispatchable = HashSet::new();
    for s in plan {
        match s.status {
            StepStatus::Completed => {
                completed.insert(s.id.clone());
            }
            StepStatus::Failed | StepStatus::Denied | StepStatus::AwaitingApproval => {
                non_dispatchable.insert(s.id.clone());
            }
            _ => {}
        }
    }
    (completed, non_dispatchable)
}

fn collapse_values(values: Vec<String>) -> String {
    match values.len() {
        0 => String::new(),
        1 => values.into_iter().next().expect("len checked"),
        _ => serde_json::to_string(&values).unwrap_or_else(|_| "[]".to_string()),
    }
}

impl Orchestrator {
    pub async fn run_task(self: &Arc<Self>, task_id: String) {
        let log_id = task_id.clone();
        {
            let mut running = self.running_tasks.lock().await;
            if running.contains(&task_id) {
                info!(task_id = %log_id, "runTask already active, skipping");
                return;
            }
            running.insert(task_id.clone());
        }

        let _guard = scopeguard::guard(task_id.clone(), |id| {
            let running = self.running_tasks.clone();
            tokio::spawn(async move {
                running.lock().await.remove(&id);
            });
        });

        if let Err(e) = self
            .store
            .update(&task_id, |s| {
                s.status = Status::Running;
                Ok(())
            })
            .await
        {
            error!(task_id = %log_id, err = %e, "failed to mark task running");
            return;
        }

        let mut state = match self.store.get(&task_id).await {
            Ok(s) => s,
            Err(e) => {
                error!(task_id = %log_id, err = %e, "failed to load task state");
                self.fail_task(&task_id, "failed to load task state").await;
                return;
            }
        };

        let mut splice_counter = 0u32;

        loop {
            let g = match build_dag(&state.plan) {
                Ok(g) => g,
                Err(e) => {
                    error!(task_id = %log_id, err = %e, "invalid plan DAG");
                    self.fail_task(&task_id, &format!("invalid plan DAG: {e}"))
                        .await;
                    return;
                }
            };

            let (completed, non_dispatchable) = plan_status_sets(&state.plan);

            let ready_ids = g.ready(&completed);
            let dispatchable: Vec<String> = ready_ids
                .into_iter()
                .filter(|id| !non_dispatchable.contains(id))
                .collect();

            if dispatchable.is_empty() {
                break;
            }

            info!(task_id = %log_id, batch_size = dispatchable.len(), "dispatching batch");

            let mut handles = Vec::new();
            for step_id in &dispatchable {
                if let Some(idx) = step_index(&state.plan, step_id) {
                    let orch = Arc::clone(self);
                    let state_clone = state.clone();
                    let sid = step_id.clone();
                    let tid = task_id.clone();
                    handles.push(tokio::spawn(async move {
                        if let Err(e) = orch.run_step(&state_clone, idx).await {
                            warn!(step_id = %sid, err = %e, "step failed");
                            let err_msg = e.to_string();
                            let s = sid.clone();
                            if let Err(e) = orch
                                .store
                                .update(&tid, |st| {
                                    if let Some(i) = step_index(&st.plan, &s) {
                                        if st.plan[i].status == StepStatus::Running {
                                            st.plan[i].status = StepStatus::Failed;
                                            st.plan[i].error = err_msg.clone();
                                        }
                                    }
                                    Ok(())
                                })
                                .await
                            {
                                error!(task_id = %tid, step_id = %sid, err = %e, "failed to mark step as failed");
                            }
                        }
                    }));
                }
            }
            for h in handles {
                if let Err(e) = h.await {
                    error!(task_id = %log_id, err = %e, "step join handle failed");
                }
            }

            state = match self.store.get(&task_id).await {
                Ok(s) => s,
                Err(e) => {
                    error!(task_id = %log_id, err = %e, "state reload failed");
                    self.fail_task(&task_id, "state reload failed").await;
                    return;
                }
            };

            // Handle splices
            for step_id in &dispatchable {
                if let Some(idx) = step_index(&state.plan, step_id) {
                    let step = &state.plan[idx];
                    if step.status != StepStatus::Completed {
                        continue;
                    }
                    if let Ok(meta) = self.registry.get(&step.agent_type) {
                        if meta.splice {
                            splice_counter += 1;
                            if let Err(e) = self.handle_splice(&state, step, splice_counter).await {
                                warn!(step_id = %step_id, err = %e, "splice failed");
                            }
                        }
                    }
                }
            }

            state = match self.store.get(&task_id).await {
                Ok(s) => s,
                Err(e) => {
                    error!(task_id = %log_id, err = %e, "state reload after splice failed");
                    self.fail_task(&task_id, "state reload failed").await;
                    return;
                }
            };

            // Check for approval parking
            let has_awaiting = state
                .plan
                .iter()
                .any(|s| s.status == StepStatus::AwaitingApproval);
            if has_awaiting {
                if let Ok(next_g) = build_dag(&state.plan) {
                    let (next_completed, next_non) = plan_status_sets(&state.plan);
                    let next_ready = next_g.ready(&next_completed);
                    let can_dispatch = next_ready.iter().any(|id| !next_non.contains(id));
                    if !can_dispatch {
                        if let Err(e) = self
                            .store
                            .update(&task_id, |s| {
                                s.status = Status::AwaitingApproval;
                                Ok(())
                            })
                            .await
                        {
                            error!(task_id = %log_id, err = %e, "failed to park task for approval");
                        }
                        info!(task_id = %log_id, "task parked, awaiting approval");
                        return;
                    }
                }
            }
        }

        // Terminal state
        let has_awaiting = state
            .plan
            .iter()
            .any(|s| s.status == StepStatus::AwaitingApproval);
        if has_awaiting {
            if let Err(e) = self
                .store
                .update(&task_id, |s| {
                    s.status = Status::AwaitingApproval;
                    Ok(())
                })
                .await
            {
                error!(task_id = %log_id, err = %e, "failed to park task for approval");
            }
            info!(task_id = %log_id, "task parked, awaiting approval");
            return;
        }

        let _has_failed = state
            .plan
            .iter()
            .any(|s| s.status == StepStatus::Failed || s.status == StepStatus::Denied);
        let all_completed = state.plan.iter().all(|s| s.status == StepStatus::Completed);

        if all_completed {
            if let Err(e) = self
                .store
                .update(&task_id, |s| {
                    s.status = Status::Completed;
                    Ok(())
                })
                .await
            {
                error!(task_id = %log_id, err = %e, "failed to mark task completed");
            }
        } else {
            self.fail_task(&task_id, "one or more steps failed or were denied")
                .await;
        }

        if let Err(e) = self
            .store
            .append_audit(&mut AuditEvent {
                task_id: task_id.clone(),
                event_type: if all_completed {
                    EventType::TaskCompleted
                } else {
                    EventType::TaskFailed
                },
                timestamp: Utc::now(),
                ..Default::default()
            })
            .await
        {
            warn!(task_id = %log_id, err = %e, "failed to write terminal audit event");
        }

        info!(task_id = %log_id, status = if all_completed { "completed" } else { "failed" }, "task finished");
    }

    async fn run_step(&self, state: &TaskState, step_idx: usize) -> Result<(), anyhow::Error> {
        let step = &state.plan[step_idx];
        let task_id = &state.task_id;
        let step_id = step.id.clone();

        info!(task_id, step_id = %step_id, agent_type = %step.agent_type, "starting step");

        let now = Utc::now();
        self.store
            .update(task_id, |s| {
                if let Some(idx) = step_index(&s.plan, &step_id) {
                    s.plan[idx].status = StepStatus::Running;
                    s.plan[idx].started_at = Some(now);
                    s.current_step = idx;
                }
                Ok(())
            })
            .await?;

        if let Err(e) = self
            .store
            .append_audit(&mut AuditEvent {
                task_id: task_id.clone(),
                step_id: step_id.clone(),
                event_type: EventType::StepStarted,
                timestamp: Utc::now(),
                policy_target: step.agent_type.clone(),
                ..Default::default()
            })
            .await
        {
            warn!(task_id, step_id = %step_id, err = %e, "audit: step-started write failed");
        }

        let meta = self.registry.get(&step.agent_type)?;

        let prior_results = self.collect_prior_results(state, &step.id);
        let context_taint = self.compute_context_taint(state, &step.id);

        let policy_result = {
            let mut policy = self
                .policy
                .lock()
                .map_err(|e| anyhow::anyhow!("policy lock poisoned: {e}"))?;
            let enriched = enrich_params(&step.params, &meta.enrichments);
            let input = serde_json::json!({
                "step": {
                    "id": step.id,
                    "index": step_idx,
                    "agent_type": step.agent_type,
                    "params": enriched,
                },
                "agent": {
                    "wasm_name": meta.wasm_name,
                    "capability": meta.capability,
                    "host_functions": meta.host_functions,
                    "declassifies": meta.declassifies,
                },
                "task": {
                    "id": task_id,
                    "type": state.context.get("type").unwrap_or(&String::new()),
                    "context": state.context,
                    "created_at": state.created_at.to_rfc3339(),
                },
                "plan": {
                    "total_steps": state.plan.len(),
                    "completed_steps": state.plan.iter().filter(|s| s.status == StepStatus::Completed).count(),
                },
                "prior_results": prior_results,
                "context_taint": context_taint,
            });
            policy.evaluate_step(input)?
        };

        if !policy_result.permitted {
            let deny_msg = policy_result.deny_message.as_deref().unwrap_or("denied");
            let deny_code = policy_result.deny_code.clone().unwrap_or_default();
            self.store
                .update(task_id, |s| {
                    if let Some(idx) = step_index(&s.plan, &step_id) {
                        s.plan[idx].status = StepStatus::Denied;
                        s.plan[idx].error = deny_msg.to_string();
                    }
                    Ok(())
                })
                .await?;
            if let Err(e) = self
                .store
                .append_audit(&mut AuditEvent {
                    task_id: task_id.clone(),
                    step_id: step_id.clone(),
                    event_type: EventType::PolicyDeny,
                    timestamp: Utc::now(),
                    policy_target: step.agent_type.clone(),
                    policy_capability: meta.capability.clone(),
                    policy_deny_code: deny_code,
                    policy_deny_msg: deny_msg.to_string(),
                    ..Default::default()
                })
                .await
            {
                warn!(task_id, step_id = %step_id, err = %e, "audit: policy-deny write failed");
            }
            anyhow::bail!("policy denied: {deny_msg}");
        }

        if policy_result.requires_approval && step.approved_by.is_empty() {
            let reason = if policy_result.approval_reason.is_empty() {
                "policy requires approval".to_string()
            } else {
                policy_result.approval_reason.clone()
            };
            self.store
                .update(task_id, |s| {
                    if let Some(idx) = step_index(&s.plan, &step_id) {
                        s.plan[idx].status = StepStatus::AwaitingApproval;
                        s.plan[idx].approval_reason = reason.clone();
                    }
                    Ok(())
                })
                .await?;

            if let Err(e) = self
                .store
                .append_audit(&mut AuditEvent {
                    task_id: task_id.clone(),
                    step_id: step_id.clone(),
                    event_type: EventType::StepAwaitingApproval,
                    message: reason.clone(),
                    timestamp: Utc::now(),
                    ..Default::default()
                })
                .await
            {
                warn!(task_id, step_id = %step_id, err = %e, "audit: awaiting-approval write failed");
            }

            self.publish_approval_needed(task_id, &step_id, &step.agent_type, &reason)
                .await;

            if self.approval_timeout_sec > 0 {
                let store = self.store.clone();
                let running_tasks = self.running_tasks.clone();
                let tid = task_id.to_string();
                let sid = step_id.clone();
                let timeout = self.approval_timeout_sec;
                tokio::spawn(async move {
                    tokio::time::sleep(std::time::Duration::from_secs(timeout)).await;
                    let still_waiting = store
                        .get(&tid)
                        .await
                        .ok()
                        .and_then(|s| {
                            s.plan
                                .iter()
                                .find(|st| st.id == sid)
                                .map(|st| st.status == StepStatus::AwaitingApproval)
                        })
                        .unwrap_or(false);

                    if still_waiting {
                        let s = sid.clone();
                        if let Err(e) = store
                            .update(&tid, |state| {
                                if let Some(idx) = state.plan.iter().position(|st| st.id == s) {
                                    state.plan[idx].status = StepStatus::Denied;
                                    state.plan[idx].error =
                                        format!("approval timed out after {timeout}s");
                                }
                                Ok(())
                            })
                            .await
                        {
                            tracing::error!(task_id = %tid, step_id = %sid, err = %e, "failed to deny timed-out step");
                        }
                        running_tasks.lock().await.remove(&tid);
                        warn!(task_id = %tid, step_id = %sid, "approval timed out, step denied");
                    }
                });
            }

            info!(task_id, step_id = %step_id, reason = %reason, "step awaiting approval");
            return Ok(());
        }

        let mut opts = PluginOpts {
            max_mem_pages: self.plugin_max_mem_pages,
            timeout: self.plugin_timeout,
            ..Default::default()
        };

        if !policy_result.allowed_hosts.is_empty() {
            opts.allowed_hosts = policy_result.allowed_hosts;
        }
        if let Some(v) = policy_result.max_memory_pages {
            opts.max_mem_pages = v as u64;
        }
        if let Some(v) = policy_result.max_http_bytes {
            opts.max_http_bytes = Some(v);
        }
        if let Some(v) = policy_result.timeout_sec {
            opts.timeout = Duration::from_secs(v as u64);
        }
        if !policy_result.config.is_empty() {
            opts.config = policy_result.config;
        }
        if !policy_result.allowed_paths.is_empty() {
            opts.allowed_paths = policy_result.allowed_paths;
        }

        let host_fn_names = if !policy_result.host_functions.is_empty() {
            policy_result.host_functions
        } else {
            meta.host_functions.clone()
        };
        opts.host_fn_names = host_fn_names;

        let input_payload = build_payload(&meta, state, step);
        let input_context = self.build_step_context(state, &step.id);

        let task_input = TaskInput {
            task_id: task_id.clone(),
            step_id: step_id.clone(),
            payload: input_payload.clone(),
            context: input_context,
        };

        self.store
            .put_payload(&step.input_key, &input_payload)
            .await?;

        let sanitized_paths = sanitize_allowed_paths(&opts.allowed_paths)
            .map_err(|e| anyhow::anyhow!("invalid allowed_paths: {e}"))?;
        let mut wasi_builder = wasmtime_wasi::WasiCtxBuilder::new();
        for (host_path, guest_path) in &sanitized_paths {
            wasi_builder
                .preopened_dir(
                    host_path,
                    guest_path,
                    wasmtime_wasi::DirPerms::all(),
                    wasmtime_wasi::FilePerms::all(),
                )
                .map_err(|e| {
                    anyhow::anyhow!("preopened_dir({host_path:?} -> {guest_path:?}): {e}")
                })?;
        }
        let wasi_ctx = wasi_builder.build();
        let allowed_hosts_set: std::collections::HashSet<String> =
            opts.allowed_hosts.iter().cloned().collect();
        let host_state = HostState {
            llm: self.llm_state.clone(),
            kv: self.kv_state.clone(),
            exec: self.exec_state.clone(),
            sandbox: self.sandbox_state.clone(),
            email: self.email_state.clone(),
            config: ConfigState {
                values: opts.config.clone(),
            },
            step_meta: StepMeta {
                task_id: task_id.clone(),
                step_id: step_id.clone(),
                agent_type: step.agent_type.clone(),
            },
            wasi_ctx,
            http_ctx: wasmtime_wasi_http::WasiHttpCtx::new(),
            resource_table: wasmtime_wasi::ResourceTable::new(),
            allowed_hosts: allowed_hosts_set,
            store_limits: wasmtime::StoreLimits::default(),
        };

        let engine = self.engine.clone();
        let wasm_name = meta.wasm_name.clone();
        let output = tokio::task::spawn_blocking(move || {
            engine.invoke_agent(&wasm_name, &task_input, opts, host_state)
        })
        .await??;

        self.store
            .put_payload(&step.output_key, &output.payload)
            .await?;

        // Compute taint for this step's output:
        // start with agent's declared output_taint, union ancestor taint,
        // then subtract any labels the agent declassifies.
        let mut step_taint: HashSet<String> = meta.output_taint.iter().cloned().collect();
        {
            let ancestor_taint = self.compute_context_taint(state, &step.id);
            step_taint.extend(ancestor_taint);
        }
        let declassified: Vec<String> = meta
            .declassifies
            .iter()
            .filter(|l| step_taint.remove(l.as_str()))
            .cloned()
            .collect();
        let step_taint_vec: Vec<String> = step_taint.into_iter().collect();

        let fin = Utc::now();
        let output_key = step.output_key.clone();
        let output_payload = output.payload.clone();
        let taint_for_store = step_taint_vec.clone();
        let taint_output_key = step.output_key.clone();
        self.store
            .update(task_id, |s| {
                if let Some(idx) = step_index(&s.plan, &step_id) {
                    s.plan[idx].status = StepStatus::Completed;
                    s.plan[idx].completed_at = Some(fin);
                    s.results.insert(output_key.clone(), output_payload.clone());
                }
                if !taint_for_store.is_empty() {
                    s.taint
                        .insert(taint_output_key.clone(), taint_for_store.clone());
                }
                Ok(())
            })
            .await?;

        if !declassified.is_empty() {
            if let Err(e) = self
                .store
                .append_audit(&mut AuditEvent {
                    task_id: task_id.clone(),
                    step_id: step_id.clone(),
                    event_type: EventType::TaintDeclassified,
                    timestamp: Utc::now(),
                    policy_target: step.agent_type.clone(),
                    message: format!("declassified: {:?}", declassified),
                    ..Default::default()
                })
                .await
            {
                warn!(task_id, step_id = %step_id, err = %e, "audit: taint-declassified write failed");
            }
        }

        if let Err(e) = self
            .store
            .append_audit(&mut AuditEvent {
                task_id: task_id.clone(),
                step_id: step_id.clone(),
                event_type: EventType::StepCompleted,
                timestamp: Utc::now(),
                component_ref: meta.wasm_name.clone(),
                policy_target: step.agent_type.clone(),
                policy_capability: meta.capability.clone(),
                ..Default::default()
            })
            .await
        {
            warn!(task_id, step_id = %step_id, err = %e, "audit: step-completed write failed");
        }

        info!(task_id, step_id = %step_id, agent_type = %step.agent_type, "step completed");
        Ok(())
    }

    fn collect_ancestor_outputs(
        &self,
        state: &TaskState,
        step_id: &str,
    ) -> Vec<(String, Vec<String>)> {
        let g = match build_dag(&state.plan) {
            Ok(g) => g,
            Err(_) => return Vec::new(),
        };

        let ancestor_ids = g.ancestors(step_id);
        let ancestor_set: HashSet<&str> = ancestor_ids.iter().map(|s| s.as_str()).collect();

        let mut entries: Vec<(String, Vec<String>)> = Vec::new();
        let mut seen: HashMap<String, usize> = HashMap::new();

        for s in &state.plan {
            if !ancestor_set.contains(s.id.as_str()) {
                continue;
            }
            let v = match state.results.get(&s.output_key) {
                Some(v) => v.clone(),
                None => continue,
            };
            let key = self
                .registry
                .get(&s.agent_type)
                .map(|m| m.context_key.clone())
                .unwrap_or_else(|_| format!("{}_result", s.agent_type));

            if let Some(&idx) = seen.get(&key) {
                entries[idx].1.push(v);
            } else {
                seen.insert(key.clone(), entries.len());
                entries.push((key, vec![v]));
            }
        }

        entries
    }

    fn collect_prior_results(&self, state: &TaskState, step_id: &str) -> HashMap<String, String> {
        self.collect_ancestor_outputs(state, step_id)
            .into_iter()
            .map(|(key, values)| (key, collapse_values(values)))
            .collect()
    }

    fn build_step_context(&self, state: &TaskState, step_id: &str) -> Vec<KvPair> {
        let ancestor_outputs = self.collect_ancestor_outputs(state, step_id);
        let ancestor_taint = self.collect_ancestor_taint(state, step_id);

        ancestor_outputs
            .into_iter()
            .map(|(key, values)| {
                let taint_labels = ancestor_taint.get(&key).cloned();
                KvPair {
                    key,
                    val: collapse_values(values),
                    taint: taint_labels,
                }
            })
            .collect()
    }

    fn collect_ancestor_taint(
        &self,
        state: &TaskState,
        step_id: &str,
    ) -> HashMap<String, Vec<String>> {
        let g = match build_dag(&state.plan) {
            Ok(g) => g,
            Err(_) => return HashMap::new(),
        };

        let ancestor_ids = g.ancestors(step_id);
        let ancestor_set: HashSet<&str> = ancestor_ids.iter().map(|s| s.as_str()).collect();

        // Labels shielded by completed declassifying ancestors.
        let mut shielded: HashSet<String> = HashSet::new();
        for s in &state.plan {
            if !ancestor_set.contains(s.id.as_str()) {
                continue;
            }
            if let Ok(meta) = self.registry.get(&s.agent_type) {
                if !meta.declassifies.is_empty() && s.status == StepStatus::Completed {
                    shielded.extend(meta.declassifies.iter().cloned());
                }
            }
        }

        let mut result: HashMap<String, HashSet<String>> = HashMap::new();

        for s in &state.plan {
            if !ancestor_set.contains(s.id.as_str()) {
                continue;
            }
            if state.results.get(&s.output_key).is_none() {
                continue;
            }
            let key = self
                .registry
                .get(&s.agent_type)
                .map(|m| m.context_key.clone())
                .unwrap_or_else(|_| format!("{}_result", s.agent_type));

            if let Some(labels) = state.taint.get(&s.output_key) {
                for label in labels {
                    if !shielded.contains(label) {
                        result.entry(key.clone()).or_default().insert(label.clone());
                    }
                }
            }
        }

        result
            .into_iter()
            .map(|(k, v)| (k, v.into_iter().collect()))
            .collect()
    }

    fn compute_context_taint(&self, state: &TaskState, step_id: &str) -> Vec<String> {
        let g = match build_dag(&state.plan) {
            Ok(g) => g,
            Err(_) => return Vec::new(),
        };

        let ancestor_ids = g.ancestors(step_id);
        let ancestor_set: HashSet<&str> = ancestor_ids.iter().map(|s| s.as_str()).collect();

        // Collect labels that declassifying ancestors absorb from their
        // own upstream ancestors. A declassifier acts as a taint barrier:
        // downstream steps should not see the labels it stripped, even from
        // the declassifier's own ancestors.
        let mut shielded: HashSet<String> = HashSet::new();
        for s in &state.plan {
            if !ancestor_set.contains(s.id.as_str()) {
                continue;
            }
            if let Ok(meta) = self.registry.get(&s.agent_type) {
                if !meta.declassifies.is_empty() && s.status == StepStatus::Completed {
                    shielded.extend(meta.declassifies.iter().cloned());
                }
            }
        }

        let mut all_taint: HashSet<String> = HashSet::new();
        for s in &state.plan {
            if !ancestor_set.contains(s.id.as_str()) {
                continue;
            }
            if let Some(labels) = state.taint.get(&s.output_key) {
                for label in labels {
                    if !shielded.contains(label) {
                        all_taint.insert(label.clone());
                    }
                }
            }
        }

        let mut sorted: Vec<String> = all_taint.into_iter().collect();
        sorted.sort();
        sorted
    }

    async fn handle_splice(
        &self,
        state: &TaskState,
        step: &Step,
        counter: u32,
    ) -> Result<(), anyhow::Error> {
        let output_json = state
            .results
            .get(&step.output_key)
            .ok_or_else(|| anyhow::anyhow!("splice step output not found"))?;

        #[derive(Deserialize)]
        struct SpliceOutput {
            #[serde(default)]
            agent_type: String,
            #[serde(default)]
            skill: String,
            #[serde(default)]
            params: HashMap<String, String>,
        }

        let splice: SpliceOutput = serde_json::from_str(output_json)?;
        let agent_type = if !splice.agent_type.is_empty() {
            splice.agent_type
        } else {
            splice.skill
        };

        if agent_type.is_empty() || agent_type == "direct-answer" {
            return Ok(());
        }

        let g = build_dag(&state.plan)?;
        let dependents = g.children(&step.id);

        let new_step_id = format!("{}-splice-{counter}", state.task_id);
        let new_step = Step {
            id: new_step_id.clone(),
            agent_type,
            input_key: format!("{new_step_id}.input"),
            output_key: format!("{new_step_id}.output"),
            status: StepStatus::Pending,
            depends_on: vec![step.id.clone()],
            params: splice.params,
            ..Default::default()
        };

        let parent_id = step.id.clone();
        self.store
            .update(&state.task_id, |s| {
                s.plan.push(new_step.clone());
                for p in &mut s.plan {
                    if dependents.contains(&p.id) {
                        if let Some(pos) = p.depends_on.iter().position(|d| d == &parent_id) {
                            p.depends_on[pos] = new_step_id.clone();
                        }
                    }
                }
                Ok(())
            })
            .await?;

        Ok(())
    }

    async fn publish_approval_needed(
        &self,
        task_id: &str,
        step_id: &str,
        agent_type: &str,
        reason: &str,
    ) {
        if self.approval_webhook_url.is_empty() {
            return;
        }
        let payload = serde_json::json!({
            "task_id": task_id,
            "step_id": step_id,
            "agent_type": agent_type,
            "reason": reason,
        });
        let url = self.approval_webhook_url.clone();
        let body = payload.to_string();
        tokio::spawn(async move {
            let client = reqwest::Client::new();
            if let Err(e) = client
                .post(&url)
                .header("Content-Type", "application/json")
                .body(body)
                .send()
                .await
            {
                tracing::error!(url = %url, err = %e, "approval webhook POST failed");
            }
        });
    }

    async fn fail_task(&self, task_id: &str, reason: &str) {
        let reason_owned = reason.to_string();
        if let Err(e) = self
            .store
            .update(task_id, |s| {
                s.status = Status::Failed;
                s.error = reason_owned.clone();
                Ok(())
            })
            .await
        {
            error!(task_id, err = %e, "failed to mark task as failed");
        }
        if let Err(e) = self
            .store
            .append_audit(&mut AuditEvent {
                task_id: task_id.to_string(),
                event_type: EventType::TaskFailed,
                message: reason.to_string(),
                timestamp: Utc::now(),
                ..Default::default()
            })
            .await
        {
            warn!(task_id, err = %e, "audit: task-failed write failed");
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use wasm_af_taskstate::{Step, StepStatus};

    fn make_step(id: &str, agent: &str, deps: &[&str]) -> Step {
        Step {
            id: id.into(),
            agent_type: agent.into(),
            input_key: format!("{id}.input"),
            output_key: format!("{id}.output"),
            depends_on: deps.iter().map(|s| s.to_string()).collect(),
            ..Default::default()
        }
    }

    fn make_step_with_status(id: &str, status: StepStatus) -> Step {
        Step {
            id: id.into(),
            agent_type: "test".into(),
            status,
            ..Default::default()
        }
    }

    // ---- build_dag ----

    #[test]
    fn build_dag_linear_chain() {
        let plan = vec![
            make_step("a", "router", &[]),
            make_step("b", "shell", &["a"]),
            make_step("c", "responder", &["b"]),
        ];
        let g = build_dag(&plan).unwrap();
        let ready = g.ready(&HashSet::new());
        assert_eq!(ready, vec!["a".to_string()]);
    }

    #[test]
    fn build_dag_parallel_fan_out() {
        let plan = vec![
            make_step("a", "router", &[]),
            make_step("b", "shell", &["a"]),
            make_step("c", "memory", &["a"]),
        ];
        let g = build_dag(&plan).unwrap();
        let completed: HashSet<String> = ["a".into()].into();
        let mut ready = g.ready(&completed);
        ready.sort();
        assert_eq!(ready, vec!["b".to_string(), "c".to_string()]);
    }

    #[test]
    fn build_dag_diamond_join() {
        let plan = vec![
            make_step("a", "router", &[]),
            make_step("b", "shell", &["a"]),
            make_step("c", "memory", &["a"]),
            make_step("d", "responder", &["b", "c"]),
        ];
        let g = build_dag(&plan).unwrap();
        let completed: HashSet<String> = ["a".into(), "b".into()].into();
        let ready = g.ready(&completed);
        assert_eq!(
            ready,
            vec!["c".to_string()],
            "d should wait for both b and c"
        );
    }

    #[test]
    fn build_dag_empty_plan() {
        let g = build_dag(&[]).unwrap();
        assert!(g.ready(&HashSet::new()).is_empty());
    }

    #[test]
    fn build_dag_unknown_dependency_errors() {
        let plan = vec![make_step("a", "shell", &["ghost"])];
        assert!(build_dag(&plan).is_err());
    }

    #[test]
    fn build_dag_single_step_no_deps() {
        let plan = vec![make_step("only", "router", &[])];
        let g = build_dag(&plan).unwrap();
        let ready = g.ready(&HashSet::new());
        assert_eq!(ready, vec!["only".to_string()]);
    }

    #[test]
    fn build_dag_all_independent() {
        let plan = vec![
            make_step("a", "shell", &[]),
            make_step("b", "memory", &[]),
            make_step("c", "router", &[]),
        ];
        let g = build_dag(&plan).unwrap();
        let mut ready = g.ready(&HashSet::new());
        ready.sort();
        assert_eq!(
            ready,
            vec!["a".to_string(), "b".to_string(), "c".to_string()]
        );
    }

    // ---- step_index ----

    #[test]
    fn step_index_first_element() {
        let plan = vec![make_step("a", "router", &[]), make_step("b", "shell", &[])];
        assert_eq!(step_index(&plan, "a"), Some(0));
    }

    #[test]
    fn step_index_second_element() {
        let plan = vec![make_step("a", "router", &[]), make_step("b", "shell", &[])];
        assert_eq!(step_index(&plan, "b"), Some(1));
    }

    #[test]
    fn step_index_not_found() {
        let plan = vec![make_step("a", "router", &[])];
        assert_eq!(step_index(&plan, "ghost"), None);
    }

    #[test]
    fn step_index_empty_plan() {
        assert_eq!(step_index(&[], "a"), None);
    }

    // ---- plan_status_sets ----

    #[test]
    fn plan_status_sets_empty() {
        let (completed, non_disp) = plan_status_sets(&[]);
        assert!(completed.is_empty());
        assert!(non_disp.is_empty());
    }

    #[test]
    fn plan_status_sets_completed() {
        let plan = vec![
            make_step_with_status("a", StepStatus::Completed),
            make_step_with_status("b", StepStatus::Completed),
        ];
        let (completed, non_disp) = plan_status_sets(&plan);
        assert_eq!(completed.len(), 2);
        assert!(completed.contains("a"));
        assert!(completed.contains("b"));
        assert!(non_disp.is_empty());
    }

    #[test]
    fn plan_status_sets_failed_is_non_dispatchable() {
        let plan = vec![make_step_with_status("a", StepStatus::Failed)];
        let (completed, non_disp) = plan_status_sets(&plan);
        assert!(completed.is_empty());
        assert!(non_disp.contains("a"));
    }

    #[test]
    fn plan_status_sets_denied_is_non_dispatchable() {
        let plan = vec![make_step_with_status("a", StepStatus::Denied)];
        let (completed, non_disp) = plan_status_sets(&plan);
        assert!(completed.is_empty());
        assert!(non_disp.contains("a"));
    }

    #[test]
    fn plan_status_sets_awaiting_is_non_dispatchable() {
        let plan = vec![make_step_with_status("a", StepStatus::AwaitingApproval)];
        let (completed, non_disp) = plan_status_sets(&plan);
        assert!(completed.is_empty());
        assert!(non_disp.contains("a"));
    }

    #[test]
    fn plan_status_sets_pending_and_running_in_neither_set() {
        let plan = vec![
            make_step_with_status("a", StepStatus::Pending),
            make_step_with_status("b", StepStatus::Running),
        ];
        let (completed, non_disp) = plan_status_sets(&plan);
        assert!(completed.is_empty());
        assert!(non_disp.is_empty());
    }

    #[test]
    fn plan_status_sets_mixed_statuses() {
        let plan = vec![
            make_step_with_status("a", StepStatus::Completed),
            make_step_with_status("b", StepStatus::Failed),
            make_step_with_status("c", StepStatus::Pending),
            make_step_with_status("d", StepStatus::AwaitingApproval),
            make_step_with_status("e", StepStatus::Running),
            make_step_with_status("f", StepStatus::Denied),
        ];
        let (completed, non_disp) = plan_status_sets(&plan);
        assert_eq!(completed, HashSet::from(["a".into()]));
        assert_eq!(
            non_disp,
            HashSet::from(["b".into(), "d".into(), "f".into()])
        );
    }

    // ---- collapse_values ----

    #[test]
    fn collapse_single_value() {
        assert_eq!(collapse_values(vec!["hello".into()]), "hello");
    }

    #[test]
    fn collapse_multiple_values_produces_json_array() {
        let result = collapse_values(vec!["a".into(), "b".into(), "c".into()]);
        let parsed: Vec<String> = serde_json::from_str(&result).unwrap();
        assert_eq!(parsed, vec!["a", "b", "c"]);
    }

    #[test]
    fn collapse_two_values_produces_json_array() {
        let result = collapse_values(vec!["x".into(), "y".into()]);
        let parsed: Vec<String> = serde_json::from_str(&result).unwrap();
        assert_eq!(parsed, vec!["x", "y"]);
    }

    #[test]
    fn collapse_single_empty_string() {
        assert_eq!(collapse_values(vec!["".into()]), "");
    }

    #[test]
    fn collapse_single_json_payload_preserved() {
        let json_str = r#"{"key":"value"}"#.to_string();
        assert_eq!(collapse_values(vec![json_str.clone()]), json_str);
    }

    // ---- integration: build_dag + plan_status_sets dispatching ----

    #[test]
    fn dispatch_logic_skips_non_dispatchable() {
        let plan = vec![
            make_step_with_status("a", StepStatus::Completed),
            Step {
                id: "b".into(),
                agent_type: "shell".into(),
                status: StepStatus::Failed,
                depends_on: vec!["a".into()],
                ..Default::default()
            },
            Step {
                id: "c".into(),
                agent_type: "memory".into(),
                status: StepStatus::Pending,
                depends_on: vec!["a".into()],
                ..Default::default()
            },
        ];
        let g = build_dag(&plan).unwrap();
        let (completed, non_disp) = plan_status_sets(&plan);
        let ready_ids = g.ready(&completed);
        let dispatchable: Vec<String> = ready_ids
            .into_iter()
            .filter(|id| !non_disp.contains(id))
            .collect();
        assert_eq!(dispatchable, vec!["c".to_string()]);
    }

    #[test]
    fn dispatch_logic_empty_when_blocked() {
        let plan = vec![
            make_step_with_status("a", StepStatus::AwaitingApproval),
            Step {
                id: "b".into(),
                agent_type: "shell".into(),
                status: StepStatus::Pending,
                depends_on: vec!["a".into()],
                ..Default::default()
            },
        ];
        let g = build_dag(&plan).unwrap();
        let (completed, non_disp) = plan_status_sets(&plan);
        let ready_ids = g.ready(&completed);
        let dispatchable: Vec<String> = ready_ids
            .into_iter()
            .filter(|id| !non_disp.contains(id))
            .collect();
        assert!(
            dispatchable.is_empty(),
            "b should be blocked by a awaiting approval"
        );
    }
}
