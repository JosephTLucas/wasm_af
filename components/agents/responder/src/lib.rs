wit_bindgen::generate!({
    world: "agent",
    path: "../../../wit/agent.wit",
});

use wasm_af::agent::host_llm;

struct ResponderAgent;

export!(ResponderAgent);

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

fn scope_email_context(raw: &str, idx: usize) -> Option<String> {
    let mut doc: serde_json::Value = serde_json::from_str(raw).ok()?;
    let emails = doc.get_mut("emails")?.as_array_mut()?;
    let email = emails.get(idx)?.clone();
    doc["emails"] = serde_json::Value::Array(vec![email]);
    doc["count"] = serde_json::Value::Number(1.into());
    serde_json::to_string(&doc).ok()
}

impl Guest for ResponderAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: ResponderInput = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        if req.message.trim().is_empty() {
            return Err("message field is required and must not be empty".to_string());
        }

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

        let llm_req = host_llm::LlmRequest {
            model: String::new(),
            messages: vec![
                host_llm::LlmMessage {
                    role: "system".to_string(),
                    content: "You are a helpful assistant. Answer the user's question \
                              using the provided context. Be concise and clear."
                        .to_string(),
                },
                host_llm::LlmMessage {
                    role: "user".to_string(),
                    content: user_content,
                },
            ],
            max_tokens: 512,
            temperature: Some(0.7),
        };

        let resp = host_llm::llm_complete(&llm_req)?;

        let raw = resp.content.trim();
        let cleaned = match raw.rfind("</think>") {
            Some(idx) => raw[idx + "</think>".len()..].trim(),
            None => raw,
        };

        let output = ResponderOutput {
            response: cleaned.to_string(),
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

    fn inbox_json(emails: &[serde_json::Value]) -> String {
        serde_json::json!({
            "emails": emails,
            "count": emails.len(),
        })
        .to_string()
    }

    fn sample_email(from: &str, subject: &str) -> serde_json::Value {
        serde_json::json!({
            "from": from,
            "subject": subject,
            "body": format!("body from {from}"),
        })
    }

    #[test]
    fn scope_email_context_picks_single_email() {
        let raw = inbox_json(&[
            sample_email("alice@a.com", "Hello"),
            sample_email("bob@b.com", "Meeting"),
            sample_email("carol@c.com", "Update"),
        ]);
        let scoped = scope_email_context(&raw, 1).unwrap();
        let v: serde_json::Value = serde_json::from_str(&scoped).unwrap();
        assert_eq!(v["count"], 1);
        let emails = v["emails"].as_array().unwrap();
        assert_eq!(emails.len(), 1);
        assert_eq!(emails[0]["from"], "bob@b.com");
    }

    #[test]
    fn scope_email_context_first_index() {
        let raw = inbox_json(&[sample_email("a@a.com", "A"), sample_email("b@b.com", "B")]);
        let scoped = scope_email_context(&raw, 0).unwrap();
        let v: serde_json::Value = serde_json::from_str(&scoped).unwrap();
        assert_eq!(v["emails"].as_array().unwrap()[0]["from"], "a@a.com");
    }

    #[test]
    fn scope_email_context_last_index() {
        let raw = inbox_json(&[sample_email("a@a.com", "A"), sample_email("b@b.com", "B")]);
        let scoped = scope_email_context(&raw, 1).unwrap();
        let v: serde_json::Value = serde_json::from_str(&scoped).unwrap();
        assert_eq!(v["emails"].as_array().unwrap()[0]["from"], "b@b.com");
    }

    #[test]
    fn scope_email_context_out_of_range() {
        let raw = inbox_json(&[sample_email("a@a.com", "A")]);
        assert!(scope_email_context(&raw, 5).is_none());
    }

    #[test]
    fn scope_email_context_invalid_json() {
        assert!(scope_email_context("not json", 0).is_none());
    }

    #[test]
    fn scope_email_context_missing_emails_key() {
        let raw = r#"{"other": "data"}"#;
        assert!(scope_email_context(raw, 0).is_none());
    }

    #[test]
    fn scope_email_context_emails_not_array() {
        let raw = r#"{"emails": "not-an-array", "count": 0}"#;
        assert!(scope_email_context(raw, 0).is_none());
    }

    #[test]
    fn scope_email_context_empty_array() {
        let raw = inbox_json(&[]);
        assert!(scope_email_context(&raw, 0).is_none());
    }

    #[test]
    fn scope_email_context_preserves_extra_fields() {
        let raw = serde_json::json!({
            "emails": [{"from": "a@a.com", "subject": "hi", "body": "yo"}],
            "count": 1,
            "folder": "inbox",
        })
        .to_string();
        let scoped = scope_email_context(&raw, 0).unwrap();
        let v: serde_json::Value = serde_json::from_str(&scoped).unwrap();
        assert_eq!(v["folder"], "inbox");
        assert_eq!(v["count"], 1);
    }
}
