use agent_types::{LlmMessage, LlmRequest, LlmResponse, TaskInput, TaskOutput};
use extism_pdk::*;

#[derive(serde::Deserialize)]
struct ResponderInput {
    #[serde(default)]
    message: String,
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

    // Collect all context (prior step outputs) into a block for the LLM.
    let mut context_parts: Vec<String> = Vec::new();
    for kv in &input.context {
        // Skip the raw memory blob — it's history already baked into the prompt.
        if kv.key == "memory_context" {
            continue;
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

    let Json(llm_resp) = unsafe {
        llm_complete(Json(llm_req)).map_err(|e| Error::msg(format!("LLM error: {e}")))?
    };

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
