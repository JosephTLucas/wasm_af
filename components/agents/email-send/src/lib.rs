wit_bindgen::generate!({
    world: "agent",
    path: "../../../wit/agent.wit",
});

use wasm_af::agent::host_email::{send_email, EmailRequest};

#[derive(serde::Deserialize)]
struct EmailSendInput {
    #[serde(default)]
    to: String,
    #[serde(default)]
    subject: String,
    #[serde(default)]
    body: String,
}

#[derive(serde::Serialize)]
struct EmailSendOutput {
    success: bool,
    message_id: String,
    error: String,
}

fn parse_recipients(to: &str) -> Vec<String> {
    to.split(',').map(|s| s.trim().to_string()).collect()
}

fn validate_send_request(to: &str, subject: &str, body: &str) -> Result<(), String> {
    if to.is_empty() {
        return Err("recipient address is required".to_string());
    }
    if subject.trim().is_empty() {
        return Err("subject is required and must not be empty".to_string());
    }
    if body.trim().is_empty() {
        return Err("body is required and must not be empty".to_string());
    }
    Ok(())
}

struct EmailSendAgent;

impl Guest for EmailSendAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: EmailSendInput = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        validate_send_request(&req.to, &req.subject, &req.body)?;

        let recipients = parse_recipients(&req.to);

        let resp = send_email(&EmailRequest {
            to: recipients,
            subject: req.subject,
            body: req.body,
            reply_to: None,
        })?;

        let output = EmailSendOutput {
            success: true,
            message_id: resp.message_id,
            error: String::new(),
        };

        let payload =
            serde_json::to_string(&output).map_err(|e| format!("serialization error: {e}"))?;

        Ok(TaskOutput {
            payload,
            metadata: vec![],
        })
    }
}

export!(EmailSendAgent);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn parse_single_recipient() {
        let r = parse_recipients("alice@example.com");
        assert_eq!(r, vec!["alice@example.com"]);
    }

    #[test]
    fn parse_multiple_recipients() {
        let r = parse_recipients("alice@a.com, bob@b.com, carol@c.com");
        assert_eq!(r, vec!["alice@a.com", "bob@b.com", "carol@c.com"]);
    }

    #[test]
    fn parse_trims_whitespace() {
        let r = parse_recipients("  alice@a.com ,  bob@b.com  ");
        assert_eq!(r, vec!["alice@a.com", "bob@b.com"]);
    }

    #[test]
    fn parse_empty_string_returns_single_empty() {
        let r = parse_recipients("");
        assert_eq!(r, vec![""]);
    }

    #[test]
    fn validate_success() {
        assert!(validate_send_request("a@b.com", "Hello", "World").is_ok());
    }

    #[test]
    fn validate_empty_to() {
        let err = validate_send_request("", "Hello", "World").unwrap_err();
        assert!(err.contains("recipient"));
    }

    #[test]
    fn validate_empty_subject() {
        let err = validate_send_request("a@b.com", "", "World").unwrap_err();
        assert!(err.contains("subject"));
    }

    #[test]
    fn validate_whitespace_only_subject() {
        let err = validate_send_request("a@b.com", "   ", "World").unwrap_err();
        assert!(err.contains("subject"));
    }

    #[test]
    fn validate_empty_body() {
        let err = validate_send_request("a@b.com", "Hello", "").unwrap_err();
        assert!(err.contains("body"));
    }

    #[test]
    fn validate_whitespace_only_body() {
        let err = validate_send_request("a@b.com", "Hello", "  \n  ").unwrap_err();
        assert!(err.contains("body"));
    }
}
