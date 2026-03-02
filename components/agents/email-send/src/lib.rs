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

struct EmailSendAgent;

impl Guest for EmailSendAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: EmailSendInput = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        if req.to.is_empty() {
            return Err("recipient address is required".to_string());
        }
        if req.subject.trim().is_empty() {
            return Err("subject is required and must not be empty".to_string());
        }
        if req.body.trim().is_empty() {
            return Err("body is required and must not be empty".to_string());
        }

        let recipients: Vec<String> = req.to.split(',').map(|s| s.trim().to_string()).collect();

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
