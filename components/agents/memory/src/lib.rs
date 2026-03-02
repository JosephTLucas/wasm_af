wit_bindgen::generate!({
    world: "agent",
    path: "../../../wit/agent.wit",
});

use wasm_af::agent::host_kv::{kv_get, kv_put};

#[derive(serde::Deserialize)]
struct MemoryInput {
    #[serde(default)]
    op: String,
    #[serde(default)]
    key: String,
    #[serde(default)]
    value: String,
}

#[derive(serde::Serialize)]
struct MemoryOutput {
    value: String,
    found: bool,
    success: bool,
}

const CONTEXT_KEY_RESPONSE: &str = "response";

struct MemoryAgent;

impl Guest for MemoryAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: MemoryInput = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        if req.key.is_empty() {
            return Err("key is required".to_string());
        }

        let output = match req.op.as_str() {
            "get" => {
                let resp = kv_get(&req.key)?;
                match resp {
                    Some(val) => MemoryOutput {
                        value: val,
                        found: true,
                        success: true,
                    },
                    None => MemoryOutput {
                        value: String::new(),
                        found: false,
                        success: true,
                    },
                }
            }
            "set" => {
                kv_put(&req.key, &req.value)?;
                MemoryOutput {
                    value: String::new(),
                    found: false,
                    success: true,
                }
            }
            "append" => {
                let append_value = if !req.value.is_empty() {
                    req.value.clone()
                } else {
                    input
                        .context
                        .iter()
                        .find(|kv| kv.key == CONTEXT_KEY_RESPONSE)
                        .map(|kv| kv.val.clone())
                        .unwrap_or_default()
                };

                if append_value.is_empty() {
                    MemoryOutput {
                        value: String::new(),
                        found: false,
                        success: true,
                    }
                } else {
                    let existing = kv_get(&req.key)?;
                    let new_value = match existing {
                        Some(val) if !val.is_empty() => format!("{val}\n{append_value}"),
                        _ => append_value,
                    };
                    kv_put(&req.key, &new_value)?;
                    MemoryOutput {
                        value: String::new(),
                        found: false,
                        success: true,
                    }
                }
            }
            _ => MemoryOutput {
                value: String::new(),
                found: false,
                success: false,
            },
        };

        let payload = serde_json::to_string(&output)
            .map_err(|e| format!("serialization error: {e}"))?;

        Ok(TaskOutput {
            payload,
            metadata: vec![],
        })
    }
}

export!(MemoryAgent);
