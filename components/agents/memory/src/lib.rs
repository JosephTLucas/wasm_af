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

fn merge_append(existing: Option<&str>, new_val: &str) -> String {
    match existing {
        Some(val) if !val.is_empty() => format!("{val}\n{new_val}"),
        _ => new_val.to_string(),
    }
}

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
                    let new_value = merge_append(existing.as_deref(), &append_value);
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

        let payload =
            serde_json::to_string(&output).map_err(|e| format!("serialization error: {e}"))?;

        Ok(TaskOutput {
            payload,
            metadata: vec![],
        })
    }
}

export!(MemoryAgent);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn merge_append_no_existing() {
        assert_eq!(merge_append(None, "hello"), "hello");
    }

    #[test]
    fn merge_append_empty_existing() {
        assert_eq!(merge_append(Some(""), "hello"), "hello");
    }

    #[test]
    fn merge_append_with_existing() {
        assert_eq!(merge_append(Some("line1"), "line2"), "line1\nline2");
    }

    #[test]
    fn merge_append_multiple() {
        let first = merge_append(None, "a");
        let second = merge_append(Some(&first), "b");
        let third = merge_append(Some(&second), "c");
        assert_eq!(third, "a\nb\nc");
    }

    #[test]
    fn merge_append_preserves_whitespace_in_values() {
        assert_eq!(merge_append(Some("  spaced  "), "  also  "), "  spaced  \n  also  ");
    }
}
