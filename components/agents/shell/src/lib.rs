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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn input_default_command_is_empty() {
        let input: ShellInput = serde_json::from_str("{}").unwrap();
        assert!(input.command.is_empty());
    }

    #[test]
    fn input_with_command() {
        let input: ShellInput =
            serde_json::from_str(r#"{"command":"ls -la /tmp"}"#).unwrap();
        assert_eq!(input.command, "ls -la /tmp");
    }

    #[test]
    fn output_success_serialization() {
        let output = ShellOutput {
            stdout: "file.txt\n".into(),
            stderr: String::new(),
            exit_code: 0,
        };
        let json: serde_json::Value = serde_json::to_value(&output).unwrap();
        assert_eq!(json["stdout"], "file.txt\n");
        assert_eq!(json["stderr"], "");
        assert_eq!(json["exit_code"], 0);
    }

    #[test]
    fn output_error_serialization() {
        let output = ShellOutput {
            stdout: String::new(),
            stderr: "command not found".into(),
            exit_code: 127,
        };
        let json: serde_json::Value = serde_json::to_value(&output).unwrap();
        assert_eq!(json["exit_code"], 127);
        assert_eq!(json["stderr"], "command not found");
    }

    #[test]
    fn output_round_trip() {
        let output = ShellOutput {
            stdout: "hello".into(),
            stderr: "warn".into(),
            exit_code: 1,
        };
        let json = serde_json::to_string(&output).unwrap();
        let v: serde_json::Value = serde_json::from_str(&json).unwrap();
        assert_eq!(v["stdout"], "hello");
        assert_eq!(v["stderr"], "warn");
        assert_eq!(v["exit_code"], 1);
    }
}
