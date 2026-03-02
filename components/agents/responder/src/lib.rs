use agent_types::{LlmMessage, LlmRequest, LlmResponse, TaskInput, TaskOutput};
use extism_pdk::*;
use serde_json::Value;

/// Extract a single email from the email-read JSON output by index.
/// Returns a narrowed JSON string containing only that email, or None
/// if the structure doesn't match (graceful fallback to full context).
fn scope_email_context(raw: &str, idx: usize) -> Option<String> {
    let mut doc: Value = serde_json::from_str(raw).ok()?;
    let emails = doc.get_mut("emails")?.as_array_mut()?;
    let email = emails.get(idx)?.clone();
    doc["emails"] = Value::Array(vec![email]);
    doc["count"] = Value::Number(1.into());
    serde_json::to_string(&doc).ok()
}

#[derive(serde::Deserialize)]
struct ResponderInput {
    #[serde(default)]
    message: String,
    #[serde(default)]
    reply_to_index: Option<String>,
}

#[derive(serde::Serialize)]
struct ResponderOutput {
    response: String,
}

#[host_fn]
extern "ExtismHost" {
    fn llm_complete(input: Json<LlmRequest>) -> Json<LlmResponse>;
}

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: ResponderInput = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    if req.message.trim().is_empty() {
        return Err(Error::msg("message field is required and must not be empty").into());
    }

    // When reply_to_index is set (parallel reply-all), narrow the email-read
    // context to just the single email this branch is responsible for. This
    // avoids feeding jailbreak content from other emails into this LLM call.
    let email_index: Option<usize> = req
        .reply_to_index
        .as_deref()
        .filter(|s| !s.is_empty())
        .and_then(|s| s.parse().ok());

    let mut context_parts: Vec<String> = Vec::new();
    for kv in &input.context {
        if kv.key == "memory_context" {
            continue;
        }
        if let Some(idx) = email_index {
            if kv.key == "skill_output" {
                if let Some(scoped) = scope_email_context(&kv.val, idx) {
                    context_parts.push(format!("[{}]\n{}", kv.key, scoped));
                    continue;
                }
            }
        }
        context_parts.push(format!("[{}]\n{}", kv.key, kv.val));
    }

    let user_content = if context_parts.is_empty() {
        format!(
            "User asked: {}\n\nPlease provide a helpful and concise response.",
            req.message
        )
    } else {
        format!(
            "User asked: {}\n\nAvailable information from prior steps:\n{}\n\n\
             Using the above information, provide a helpful and concise response \
             to the user's message.",
            req.message,
            context_parts.join("\n\n")
        )
    };

    let llm_req = LlmRequest {
        model: String::new(),
        messages: vec![
            LlmMessage {
                role: "system".to_string(),
                content: "You are a helpful assistant. Answer the user's question \
                          using the provided context. Be concise and clear."
                    .to_string(),
            },
            LlmMessage {
                role: "user".to_string(),
                content: user_content,
            },
        ],
        max_tokens: 512,
        temperature: Some(0.7),
    };

    let Json(llm_resp) =
        unsafe { llm_complete(Json(llm_req)).map_err(|e| Error::msg(format!("LLM error: {e}")))? };

    let raw = llm_resp.content.trim();
    let cleaned = match raw.rfind("</think>") {
        Some(idx) => raw[idx + "</think>".len()..].trim(),
        None => raw,
    };

    let output = ResponderOutput {
        response: cleaned.to_string(),
    };
    let payload = serde_json::to_string(&output)
        .map_err(|e| Error::msg(format!("serialization error: {e}")))?;

    Ok(Json(TaskOutput {
        payload,
        metadata: vec![],
    }))
}
