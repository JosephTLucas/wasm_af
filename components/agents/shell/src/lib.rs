wit_bindgen::generate!({
    world: "agent",
    path: "../../../wit/agent.wit",
});

use wasm_af::agent::host_exec;

struct ShellAgent;

export!(ShellAgent);

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

impl Guest for ShellAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: ShellInput = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        if req.command.is_empty() {
            return Err("command is required".to_string());
        }

        let resp = host_exec::exec_command(&host_exec::ExecRequest {
            command: req.command,
            args: vec![],
            working_dir: None,
        })?;

        let output = ShellOutput {
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
