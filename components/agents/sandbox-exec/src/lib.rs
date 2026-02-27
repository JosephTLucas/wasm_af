use agent_types::{TaskInput, TaskOutput};
use extism_pdk::*;

#[derive(serde::Deserialize)]
struct SandboxInput {
    #[serde(default)]
    code: String,
    #[serde(default)]
    language: String,
    #[serde(default)]
    argv: Vec<String>,
    #[serde(default)]
    stdin: String,
}

#[derive(serde::Serialize)]
struct SandboxOutput {
    stdout: String,
    stderr: String,
    exit_code: i32,
}

#[derive(serde::Serialize)]
struct SandboxExecRequest {
    code: String,
    language: String,
    #[serde(skip_serializing_if = "Vec::is_empty")]
    argv: Vec<String>,
    #[serde(skip_serializing_if = "String::is_empty")]
    stdin: String,
}

#[derive(serde::Deserialize)]
struct SandboxExecResponse {
    stdout: String,
    stderr: String,
    exit_code: i32,
}

#[host_fn]
extern "ExtismHost" {
    fn sandbox_exec(input: Json<SandboxExecRequest>) -> Json<SandboxExecResponse>;
}

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: SandboxInput = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    if req.code.is_empty() {
        return Err(Error::msg("code is required").into());
    }
    if req.language.is_empty() {
        return Err(Error::msg("language is required").into());
    }

    let Json(resp) = unsafe {
        sandbox_exec(Json(SandboxExecRequest {
            code: req.code,
            language: req.language,
            argv: req.argv,
            stdin: req.stdin,
        }))
        .map_err(|e| Error::msg(format!("sandbox_exec error: {e}")))?
    };

    let output = SandboxOutput {
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
