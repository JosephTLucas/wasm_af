use agent_types::{TaskInput, TaskOutput};
use extism_pdk::*;

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

#[derive(serde::Serialize)]
struct KvGetRequest {
    key: String,
}

#[derive(serde::Deserialize)]
struct KvGetResponse {
    value: String,
    found: bool,
}

#[derive(serde::Serialize)]
struct KvPutRequest {
    key: String,
    value: String,
}

#[derive(serde::Deserialize)]
struct KvPutResponse {
    success: bool,
}

#[host_fn]
extern "ExtismHost" {
    fn kv_get(input: Json<KvGetRequest>) -> Json<KvGetResponse>;
    fn kv_put(input: Json<KvPutRequest>) -> Json<KvPutResponse>;
}

const CONTEXT_KEY_RESPONSE: &str = "response";

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: MemoryInput = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    if req.key.is_empty() {
        return Err(Error::msg("key is required").into());
    }

    let output = match req.op.as_str() {
        "get" => {
            let Json(resp) = unsafe {
                kv_get(Json(KvGetRequest { key: req.key }))
                    .map_err(|e| Error::msg(format!("kv_get error: {e}")))?
            };
            MemoryOutput {
                value: resp.value,
                found: resp.found,
                success: true,
            }
        }
        "set" => {
            let Json(resp) = unsafe {
                kv_put(Json(KvPutRequest {
                    key: req.key,
                    value: req.value,
                }))
                .map_err(|e| Error::msg(format!("kv_put error: {e}")))?
            };
            MemoryOutput {
                value: String::new(),
                found: false,
                success: resp.success,
            }
        }
        "append" => {
            // If no explicit value, look for the responder's output in context.
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
                // Nothing to append — succeed silently.
                MemoryOutput {
                    value: String::new(),
                    found: false,
                    success: true,
                }
            } else {
                // Read existing value, concatenate, write back.
                let Json(get_resp) = unsafe {
                    kv_get(Json(KvGetRequest { key: req.key.clone() }))
                        .map_err(|e| Error::msg(format!("kv_get error: {e}")))?
                };
                let new_value = if get_resp.found && !get_resp.value.is_empty() {
                    format!("{}\n{}", get_resp.value, append_value)
                } else {
                    append_value
                };
                let Json(put_resp) = unsafe {
                    kv_put(Json(KvPutRequest {
                        key: req.key,
                        value: new_value,
                    }))
                    .map_err(|e| Error::msg(format!("kv_put error: {e}")))?
                };
                MemoryOutput {
                    value: String::new(),
                    found: false,
                    success: put_resp.success,
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
        .map_err(|e| Error::msg(format!("serialization error: {e}")))?;

    Ok(Json(TaskOutput {
        payload,
        metadata: vec![],
    }))
}
