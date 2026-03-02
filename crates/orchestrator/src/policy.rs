use serde::{Deserialize, Serialize};
use std::collections::HashMap;
use std::path::Path;

#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct PolicyResult {
    pub permitted: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub deny_code: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub deny_message: Option<String>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub allowed_hosts: Vec<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_memory_pages: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub max_http_bytes: Option<i64>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub timeout_sec: Option<i32>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub host_functions: Vec<String>,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    pub config: HashMap<String, String>,
    #[serde(default, skip_serializing_if = "HashMap::is_empty")]
    pub allowed_paths: HashMap<String, String>,
    #[serde(default)]
    pub requires_approval: bool,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub approval_reason: String,
}

pub struct OpaEvaluator {
    engine: regorus::Engine,
}

impl OpaEvaluator {
    pub fn new(
        modules: &HashMap<String, String>,
        initial_data: Option<serde_json::Value>,
    ) -> Result<Self, anyhow::Error> {
        let mut engine = regorus::Engine::new();

        for (name, source) in modules {
            engine.add_policy(name.clone(), source.clone())?;
        }

        if let Some(data) = initial_data {
            let data_str = serde_json::to_string(&data)?;
            engine.add_data(regorus::Value::from_json_str(&data_str)?)?;
        }

        Ok(OpaEvaluator { engine })
    }

    pub fn evaluate_step(
        &mut self,
        input: serde_json::Value,
    ) -> Result<PolicyResult, anyhow::Error> {
        let input_str = serde_json::to_string(&input)?;
        self.engine
            .set_input(regorus::Value::from_json_str(&input_str)?);
        let results = self
            .engine
            .eval_query("data.wasm_af.authz".to_string(), false)?;
        let value = results
            .result
            .first()
            .and_then(|r| r.expressions.first())
            .map(|e| &e.value)
            .ok_or_else(|| anyhow::anyhow!("policy returned no results"))?;
        parse_authz_result(value)
    }

    pub fn evaluate_submit(
        &mut self,
        input: serde_json::Value,
    ) -> Result<PolicyResult, anyhow::Error> {
        let input_str = serde_json::to_string(&input)?;
        self.engine
            .set_input(regorus::Value::from_json_str(&input_str)?);
        let results = self
            .engine
            .eval_query("data.wasm_af.submit".to_string(), false);

        match results {
            Ok(qr) => {
                let value = qr
                    .result
                    .first()
                    .and_then(|r| r.expressions.first())
                    .map(|e| &e.value);
                match value {
                    Some(v) => parse_submit_result(v),
                    None => Ok(PolicyResult {
                        permitted: true,
                        ..Default::default()
                    }),
                }
            }
            Err(_) => Ok(PolicyResult {
                permitted: true,
                ..Default::default()
            }),
        }
    }

    #[allow(dead_code)]
    pub fn update_data(
        &mut self,
        path: &str,
        value: serde_json::Value,
    ) -> Result<(), anyhow::Error> {
        let parts: Vec<&str> = path
            .trim_start_matches('/')
            .split('/')
            .filter(|s| !s.is_empty())
            .collect();
        let mut wrapper = value;
        for part in parts.into_iter().rev() {
            wrapper = serde_json::json!({ part: wrapper });
        }
        let data_str = serde_json::to_string(&wrapper)?;
        self.engine
            .add_data(regorus::Value::from_json_str(&data_str)?)?;
        Ok(())
    }
}

fn regorus_to_json(value: &regorus::Value) -> serde_json::Value {
    let s = value.to_json_str().unwrap_or_else(|_| "null".to_string());
    serde_json::from_str(&s).unwrap_or(serde_json::Value::Null)
}

fn parse_authz_result(value: &regorus::Value) -> Result<PolicyResult, anyhow::Error> {
    let json_val = regorus_to_json(value);
    let obj = match json_val.as_object() {
        Some(o) => o,
        None => {
            return Ok(PolicyResult {
                permitted: false,
                deny_message: Some("policy evaluation returned no results".to_string()),
                ..Default::default()
            })
        }
    };

    let allowed = obj
        .get("allow")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);

    if !allowed {
        return Ok(PolicyResult {
            permitted: false,
            deny_code: obj
                .get("deny_code")
                .and_then(|v| v.as_str())
                .map(String::from),
            deny_message: obj
                .get("deny_message")
                .and_then(|v| v.as_str())
                .map(String::from),
            ..Default::default()
        });
    }

    let mut result = PolicyResult {
        permitted: true,
        ..Default::default()
    };

    if let Some(hosts) = obj.get("allowed_hosts").and_then(|v| v.as_array()) {
        result.allowed_hosts = hosts
            .iter()
            .filter_map(|v| v.as_str().map(String::from))
            .collect();
    }
    if let Some(v) = obj.get("max_memory_pages").and_then(|v| v.as_u64()) {
        result.max_memory_pages = Some(v as u32);
    }
    if let Some(v) = obj.get("max_http_bytes").and_then(|v| v.as_i64()) {
        result.max_http_bytes = Some(v);
    }
    if let Some(v) = obj.get("timeout_sec").and_then(|v| v.as_i64()) {
        result.timeout_sec = Some(v as i32);
    }
    if let Some(fns) = obj.get("host_functions").and_then(|v| v.as_array()) {
        result.host_functions = fns
            .iter()
            .filter_map(|v| v.as_str().map(String::from))
            .collect();
    }
    if let Some(cfg) = obj.get("config").and_then(|v| v.as_object()) {
        result.config = cfg
            .iter()
            .filter_map(|(k, v)| v.as_str().map(|s| (k.clone(), s.to_string())))
            .collect();
    }
    if let Some(paths) = obj.get("allowed_paths").and_then(|v| v.as_object()) {
        result.allowed_paths = paths
            .iter()
            .filter_map(|(k, v)| v.as_str().map(|s| (k.clone(), s.to_string())))
            .collect();
    }
    if let Some(v) = obj.get("requires_approval").and_then(|v| v.as_bool()) {
        result.requires_approval = v;
    }
    if let Some(v) = obj.get("approval_reason").and_then(|v| v.as_str()) {
        result.approval_reason = v.to_string();
    }

    Ok(result)
}

fn parse_submit_result(value: &regorus::Value) -> Result<PolicyResult, anyhow::Error> {
    let json_val = regorus_to_json(value);
    let obj = match json_val.as_object() {
        Some(o) => o,
        None => {
            return Ok(PolicyResult {
                permitted: true,
                ..Default::default()
            })
        }
    };

    if !obj.contains_key("allow") {
        return Ok(PolicyResult {
            permitted: true,
            ..Default::default()
        });
    }

    let allowed = obj
        .get("allow")
        .and_then(|v| v.as_bool())
        .unwrap_or(false);

    if allowed {
        return Ok(PolicyResult {
            permitted: true,
            ..Default::default()
        });
    }

    Ok(PolicyResult {
        permitted: false,
        deny_code: obj
            .get("deny_code")
            .and_then(|v| v.as_str())
            .map(String::from),
        deny_message: obj
            .get("deny_message")
            .and_then(|v| v.as_str())
            .map(String::from),
        ..Default::default()
    })
}

pub fn load_rego_modules(path: &Path) -> Result<HashMap<String, String>, anyhow::Error> {
    let mut modules = HashMap::new();

    if path.is_file() {
        let src = std::fs::read_to_string(path)?;
        let name = path
            .file_name()
            .unwrap_or_default()
            .to_string_lossy()
            .to_string();
        modules.insert(name, src);
        return Ok(modules);
    }

    for entry in std::fs::read_dir(path)? {
        let entry = entry?;
        let p = entry.path();
        if !p.is_file() {
            continue;
        }
        let name = p
            .file_name()
            .unwrap_or_default()
            .to_string_lossy()
            .to_string();
        if !name.ends_with(".rego") || name.ends_with("_test.rego") {
            continue;
        }
        let src = std::fs::read_to_string(&p)?;
        modules.insert(name, src);
    }

    if modules.is_empty() {
        anyhow::bail!("no .rego files found in {}", path.display());
    }

    Ok(modules)
}

pub fn load_data_file(path: &Path) -> Result<serde_json::Value, anyhow::Error> {
    let bytes = std::fs::read(path)?;
    let data: serde_json::Value = serde_json::from_slice(&bytes)?;
    Ok(data)
}
