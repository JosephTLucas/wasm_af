use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::sync::RwLock;
use wasm_af_taskstate::{Step, TaskState};

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ParamEnrichment {
    pub source: String,
    pub target: String,
    pub transform: String,
}

#[derive(Debug, Clone, Default, Serialize, Deserialize)]
pub struct AgentMeta {
    pub wasm_name: String,
    pub capability: String,
    pub context_key: String,
    #[serde(default)]
    pub host_functions: Vec<String>,
    #[serde(default)]
    pub payload_fields: HashMap<String, serde_json::Value>,
    #[serde(default)]
    pub enrichments: Vec<ParamEnrichment>,
    #[serde(default)]
    pub splice: bool,
    #[serde(default)]
    pub external: bool,
}

pub struct AgentRegistry {
    agents: RwLock<HashMap<String, AgentMeta>>,
}

impl AgentRegistry {
    pub fn parse(data: &[u8]) -> Result<Self, anyhow::Error> {
        let raw: HashMap<String, AgentMeta> = serde_json::from_slice(data)?;
        for (name, meta) in &raw {
            validate_agent_meta(name, meta)?;
        }
        Ok(AgentRegistry {
            agents: RwLock::new(raw),
        })
    }

    pub fn get(&self, agent_type: &str) -> Result<AgentMeta, anyhow::Error> {
        let agents = self.agents.read().unwrap();
        agents
            .get(agent_type)
            .cloned()
            .ok_or_else(|| anyhow::anyhow!("unknown agent type {agent_type:?}"))
    }

    pub fn register(&self, name: &str, meta: AgentMeta) -> Result<(), anyhow::Error> {
        validate_agent_meta(name, &meta)?;
        let mut agents = self.agents.write().unwrap();
        agents.insert(name.to_string(), meta);
        Ok(())
    }

    pub fn remove(&self, name: &str) {
        let mut agents = self.agents.write().unwrap();
        agents.remove(name);
    }

    pub fn list(&self) -> HashMap<String, AgentMeta> {
        let agents = self.agents.read().unwrap();
        agents.clone()
    }

    #[allow(dead_code)]
    pub fn list_external(&self) -> HashMap<String, AgentMeta> {
        let agents = self.agents.read().unwrap();
        agents
            .iter()
            .filter(|(_, v)| v.external)
            .map(|(k, v)| (k.clone(), v.clone()))
            .collect()
    }

    #[allow(dead_code)]
    pub fn clear_external(&self) {
        let mut agents = self.agents.write().unwrap();
        agents.retain(|_, v| !v.external);
    }

    pub fn is_platform(&self, name: &str) -> bool {
        let agents = self.agents.read().unwrap();
        agents.get(name).map(|m| !m.external).unwrap_or(false)
    }
}

fn validate_agent_meta(name: &str, meta: &AgentMeta) -> Result<(), anyhow::Error> {
    if meta.wasm_name.is_empty() {
        anyhow::bail!("agent {name:?}: wasm_name is required");
    }
    if meta.capability.is_empty() {
        anyhow::bail!("agent {name:?}: capability is required");
    }
    if meta.context_key.is_empty() {
        anyhow::bail!("agent {name:?}: context_key is required");
    }
    Ok(())
}

/// Construct the JSON payload string for a step using the agent's payload_fields.
pub fn build_payload(meta: &AgentMeta, state: &TaskState, step: &Step) -> String {
    if meta.payload_fields.is_empty() {
        return "{}".to_string();
    }

    let mut out = serde_json::Map::new();
    for (field, spec) in &meta.payload_fields {
        match spec {
            serde_json::Value::String(ref_str) => {
                out.insert(
                    field.clone(),
                    serde_json::Value::String(resolve_field_ref(ref_str, state, step)),
                );
            }
            other => {
                out.insert(field.clone(), other.clone());
            }
        }
    }
    serde_json::to_string(&out).unwrap_or_else(|_| "{}".to_string())
}

fn resolve_field_ref(reference: &str, state: &TaskState, step: &Step) -> String {
    if let Some(key) = reference.strip_prefix("step.params.") {
        return step.params.get(key).cloned().unwrap_or_default();
    }
    if let Some(key) = reference.strip_prefix("task.context.") {
        return state.context.get(key).cloned().unwrap_or_default();
    }
    reference.to_string()
}

/// Apply enrichments (e.g., extract domain from URL) to a copy of step params.
pub fn enrich_params(
    params: &HashMap<String, String>,
    enrichments: &[ParamEnrichment],
) -> HashMap<String, String> {
    let mut out = params.clone();
    for e in enrichments {
        if let Some(src) = out.get(&e.source).cloned() {
            let val = match e.transform.as_str() {
                "domain" => extract_domain(&src),
                _ => src,
            };
            out.insert(e.target.clone(), val);
        }
    }
    out
}

fn extract_domain(raw_url: &str) -> String {
    url::Url::parse(raw_url)
        .ok()
        .and_then(|u| u.host_str().map(|h| h.to_string()))
        .unwrap_or_else(|| raw_url.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use chrono::Utc;
    use wasm_af_taskstate::Status;

    fn minimal_registry_json() -> &'static str {
        r#"{
            "shell": {
                "wasm_name": "shell",
                "capability": "shell",
                "context_key": "skill_output",
                "host_functions": ["exec_command"]
            }
        }"#
    }

    fn make_task_state() -> TaskState {
        TaskState {
            task_id: "t1".into(),
            status: Status::Running,
            plan: vec![],
            current_step: 0,
            results: HashMap::new(),
            context: HashMap::from([("message".into(), "hello".into())]),
            created_at: Utc::now(),
            updated_at: Utc::now(),
            error: String::new(),
        }
    }

    fn make_step() -> Step {
        Step {
            id: "s1".into(),
            agent_type: "shell".into(),
            params: HashMap::from([("command".into(), "ls".into())]),
            ..Default::default()
        }
    }

    // ---- Registry parsing ----

    #[test]
    fn parse_valid_registry() {
        let reg = AgentRegistry::parse(minimal_registry_json().as_bytes()).unwrap();
        let meta = reg.get("shell").unwrap();
        assert_eq!(meta.wasm_name, "shell");
        assert_eq!(meta.host_functions, vec!["exec_command"]);
    }

    #[test]
    fn parse_rejects_missing_wasm_name() {
        let json = r#"{"bad": {"wasm_name": "", "capability": "x", "context_key": "y"}}"#;
        assert!(AgentRegistry::parse(json.as_bytes()).is_err());
    }

    #[test]
    fn parse_rejects_missing_capability() {
        let json = r#"{"bad": {"wasm_name": "x", "capability": "", "context_key": "y"}}"#;
        assert!(AgentRegistry::parse(json.as_bytes()).is_err());
    }

    #[test]
    fn parse_rejects_missing_context_key() {
        let json = r#"{"bad": {"wasm_name": "x", "capability": "y", "context_key": ""}}"#;
        assert!(AgentRegistry::parse(json.as_bytes()).is_err());
    }

    // ---- Registration ----

    #[test]
    fn register_and_get() {
        let reg = AgentRegistry::parse(minimal_registry_json().as_bytes()).unwrap();
        reg.register("test-agent", AgentMeta {
            wasm_name: "test".into(),
            capability: "untrusted".into(),
            context_key: "test_result".into(),
            external: true,
            ..Default::default()
        }).unwrap();
        let meta = reg.get("test-agent").unwrap();
        assert!(meta.external);
    }

    #[test]
    fn get_unknown_agent_errors() {
        let reg = AgentRegistry::parse(minimal_registry_json().as_bytes()).unwrap();
        assert!(reg.get("nonexistent").is_err());
    }

    #[test]
    fn remove_agent() {
        let reg = AgentRegistry::parse(minimal_registry_json().as_bytes()).unwrap();
        reg.remove("shell");
        assert!(reg.get("shell").is_err());
    }

    #[test]
    fn is_platform_true_for_startup_agents() {
        let reg = AgentRegistry::parse(minimal_registry_json().as_bytes()).unwrap();
        assert!(reg.is_platform("shell"));
    }

    #[test]
    fn is_platform_false_for_external() {
        let reg = AgentRegistry::parse(minimal_registry_json().as_bytes()).unwrap();
        reg.register("ext", AgentMeta {
            wasm_name: "ext".into(),
            capability: "untrusted".into(),
            context_key: "ext_result".into(),
            external: true,
            ..Default::default()
        }).unwrap();
        assert!(!reg.is_platform("ext"));
    }

    #[test]
    fn is_platform_false_for_unknown() {
        let reg = AgentRegistry::parse(minimal_registry_json().as_bytes()).unwrap();
        assert!(!reg.is_platform("ghost"));
    }

    // ---- Payload building ----

    #[test]
    fn build_payload_empty_fields_returns_empty_json() {
        let meta = AgentMeta {
            wasm_name: "x".into(),
            capability: "y".into(),
            context_key: "z".into(),
            ..Default::default()
        };
        assert_eq!(build_payload(&meta, &make_task_state(), &make_step()), "{}");
    }

    #[test]
    fn build_payload_resolves_step_params() {
        let meta = AgentMeta {
            wasm_name: "shell".into(),
            capability: "shell".into(),
            context_key: "out".into(),
            payload_fields: HashMap::from([
                ("command".into(), serde_json::json!("step.params.command")),
            ]),
            ..Default::default()
        };
        let payload = build_payload(&meta, &make_task_state(), &make_step());
        let v: serde_json::Value = serde_json::from_str(&payload).unwrap();
        assert_eq!(v["command"], "ls");
    }

    #[test]
    fn build_payload_resolves_task_context() {
        let meta = AgentMeta {
            wasm_name: "r".into(),
            capability: "llm".into(),
            context_key: "out".into(),
            payload_fields: HashMap::from([
                ("message".into(), serde_json::json!("task.context.message")),
            ]),
            ..Default::default()
        };
        let payload = build_payload(&meta, &make_task_state(), &make_step());
        let v: serde_json::Value = serde_json::from_str(&payload).unwrap();
        assert_eq!(v["message"], "hello");
    }

    #[test]
    fn build_payload_literal_values_passed_through() {
        let meta = AgentMeta {
            wasm_name: "ws".into(),
            capability: "http".into(),
            context_key: "out".into(),
            payload_fields: HashMap::from([
                ("count".into(), serde_json::json!(5)),
            ]),
            ..Default::default()
        };
        let payload = build_payload(&meta, &make_task_state(), &make_step());
        let v: serde_json::Value = serde_json::from_str(&payload).unwrap();
        assert_eq!(v["count"], 5);
    }

    // ---- Enrichments ----

    #[test]
    fn enrich_params_extracts_domain() {
        let params = HashMap::from([("url".into(), "https://example.com/page".into())]);
        let enrichments = vec![ParamEnrichment {
            source: "url".into(),
            target: "domain".into(),
            transform: "domain".into(),
        }];
        let enriched = enrich_params(&params, &enrichments);
        assert_eq!(enriched.get("domain").unwrap(), "example.com");
    }

    #[test]
    fn enrich_params_unknown_transform_passes_through() {
        let params = HashMap::from([("key".into(), "value".into())]);
        let enrichments = vec![ParamEnrichment {
            source: "key".into(),
            target: "out".into(),
            transform: "noop".into(),
        }];
        let enriched = enrich_params(&params, &enrichments);
        assert_eq!(enriched.get("out").unwrap(), "value");
    }

    #[test]
    fn enrich_params_missing_source_skipped() {
        let params = HashMap::from([("other".into(), "val".into())]);
        let enrichments = vec![ParamEnrichment {
            source: "url".into(),
            target: "domain".into(),
            transform: "domain".into(),
        }];
        let enriched = enrich_params(&params, &enrichments);
        assert!(!enriched.contains_key("domain"));
    }

    #[test]
    fn extract_domain_from_url() {
        assert_eq!(extract_domain("https://api.brave.com/search"), "api.brave.com");
    }

    #[test]
    fn extract_domain_from_non_url() {
        assert_eq!(extract_domain("not-a-url"), "not-a-url");
    }
}
