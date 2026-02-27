use agent_types::{TaskInput, TaskOutput};
use extism_pdk::*;

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

#[derive(serde::Serialize)]
struct SendEmailRequest {
    to: String,
    subject: String,
    body: String,
}

#[derive(serde::Deserialize)]
struct SendEmailResponse {
    success: bool,
    message_id: String,
    error: String,
}

#[host_fn]
extern "ExtismHost" {
    fn send_email(input: Json<SendEmailRequest>) -> Json<SendEmailResponse>;
}

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: EmailSendInput = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    if req.to.is_empty() {
        return Err(Error::msg("recipient address is required").into());
    }

    let Json(resp) = unsafe {
        send_email(Json(SendEmailRequest {
            to: req.to,
            subject: req.subject,
            body: req.body,
        }))
        .map_err(|e| Error::msg(format!("send_email error: {e}")))?
    };

    let output = EmailSendOutput {
        success: resp.success,
        message_id: resp.message_id,
        error: resp.error,
    };

    let payload = serde_json::to_string(&output)?;
    Ok(Json(TaskOutput {
        payload,
        metadata: vec![],
    }))
}
