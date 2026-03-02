wit_bindgen::generate!({
    world: "agent",
    path: "../../../wit/agent.wit",
});

use wasm_af::agent::host_sandbox::{sandbox_exec, SandboxRequest};

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

struct SandboxAgent;

impl Guest for SandboxAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: SandboxInput = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        if req.code.is_empty() {
            return Err("code is required".to_string());
        }
        if req.language.is_empty() {
            return Err("language is required".to_string());
        }

        let stdin = if req.stdin.is_empty() {
            None
        } else {
            Some(req.stdin)
        };

        let resp = sandbox_exec(&SandboxRequest {
            language: req.language,
            code: req.code,
            args: req.argv,
            stdin,
        })?;

        let output = SandboxOutput {
            stdout: resp.stdout,
            stderr: resp.stderr,
            exit_code: resp.exit_code,
        };

        let payload =
            serde_json::to_string(&output).map_err(|e| format!("serialization error: {e}"))?;

        Ok(TaskOutput {
            payload,
            metadata: vec![],
        })
    }
}

export!(SandboxAgent);
