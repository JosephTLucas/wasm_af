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

#[derive(Debug, Clone, Serialize, Deserialize)]
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
