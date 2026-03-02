wit_bindgen::generate!({
    world: "agent",
    path: "../../../wit/agent.wit",
});

use wasm_af::agent::host_llm;

struct SummarizerAgent;

export!(SummarizerAgent);

#[derive(serde::Deserialize)]
struct SummarizerInput {
    #[serde(default)]
    query: String,
    #[serde(default)]
    model: String,
    #[serde(default)]
    max_tokens: Option<u32>,
}

#[derive(serde::Serialize)]
struct SummaryOutput {
    summary: String,
    model_used: String,
}

impl Guest for SummarizerAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: SummarizerInput = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        let mut context_parts = Vec::new();
        for kv in &input.context {
            context_parts.push(format!("[{}]\n{}", kv.key, kv.val));
        }

        let user_content = if context_parts.is_empty() {
            format!("Please provide a summary about: {}", req.query)
        } else {
            format!(
                "Summarize the following information about \"{}\":\n\n{}",
                req.query,
                context_parts.join("\n\n")
            )
        };

        let llm_req = host_llm::LlmRequest {
            model: req.model,
            messages: vec![
                host_llm::LlmMessage {
                    role: "system".to_string(),
                    content: "You are a concise summarizer. Distill the key points.".to_string(),
                },
                host_llm::LlmMessage {
                    role: "user".to_string(),
                    content: user_content,
                },
            ],
            max_tokens: req.max_tokens.unwrap_or(512),
            temperature: Some(0.3),
        };

        let resp = host_llm::llm_complete(&llm_req)?;

        let output = SummaryOutput {
            summary: resp.content,
            model_used: resp.model_used,
        };

        let payload =
            serde_json::to_string(&output).map_err(|e| format!("serialization error: {e}"))?;
        Ok(TaskOutput {
            payload,
            metadata: vec![],
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn input_all_fields_default() {
        let input: SummarizerInput = serde_json::from_str("{}").unwrap();
        assert!(input.query.is_empty());
        assert!(input.model.is_empty());
        assert!(input.max_tokens.is_none());
    }

    #[test]
    fn input_with_all_fields() {
        let input: SummarizerInput =
            serde_json::from_str(r#"{"query":"test","model":"gpt-4","max_tokens":1024}"#).unwrap();
        assert_eq!(input.query, "test");
        assert_eq!(input.model, "gpt-4");
        assert_eq!(input.max_tokens, Some(1024));
    }

    #[test]
    fn input_partial_fields() {
        let input: SummarizerInput = serde_json::from_str(r#"{"query":"summarize this"}"#).unwrap();
        assert_eq!(input.query, "summarize this");
        assert!(input.model.is_empty());
        assert!(input.max_tokens.is_none());
    }

    #[test]
    fn output_serialization() {
        let output = SummaryOutput {
            summary: "key points here".into(),
            model_used: "gpt-4".into(),
        };
        let json: serde_json::Value = serde_json::to_value(&output).unwrap();
        assert_eq!(json["summary"], "key points here");
        assert_eq!(json["model_used"], "gpt-4");
    }

    #[test]
    fn default_max_tokens_fallback_is_512() {
        let input: SummarizerInput = serde_json::from_str("{}").unwrap();
        assert_eq!(input.max_tokens.unwrap_or(512), 512);
    }
}
