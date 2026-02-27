use agent_types::{TaskInput, TaskOutput};
use extism_pdk::*;

#[derive(serde::Deserialize)]
struct ShellInput {
    #[serde(default)]
    command: String,
}

#[derive(serde::Serialize)]
struct ShellOutput {
    stdout: String,
    stderr: String,
    exit_code: i32,
}

#[derive(serde::Serialize)]
struct ExecRequest {
    command: String,
}

#[derive(serde::Deserialize)]
struct ExecResponse {
    stdout: String,
    stderr: String,
    exit_code: i32,
}

#[host_fn]
extern "ExtismHost" {
    fn exec_command(input: Json<ExecRequest>) -> Json<ExecResponse>;
}

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: ShellInput = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    if req.command.is_empty() {
        return Err(Error::msg("command is required").into());
    }

    let Json(resp) = unsafe {
        exec_command(Json(ExecRequest {
            command: req.command,
        }))
        .map_err(|e| Error::msg(format!("exec_command error: {e}")))?
    };

    let output = ShellOutput {
        stdout: resp.stdout,
        stderr: resp.stderr,
        exit_code: resp.exit_code,
    };

    let payload = serde_json::to_string(&output)
        .map_err(|e| Error::msg(format!("serialization error: {e}")))?;

    Ok(Json(TaskOutput {
        payload,
        metadata: vec![],
    }))
}
