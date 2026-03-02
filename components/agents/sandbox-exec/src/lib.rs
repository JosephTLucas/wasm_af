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

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn input_all_defaults() {
        let input: SandboxInput = serde_json::from_str("{}").unwrap();
        assert!(input.code.is_empty());
        assert!(input.language.is_empty());
        assert!(input.argv.is_empty());
        assert!(input.stdin.is_empty());
    }

    #[test]
    fn input_with_all_fields() {
        let input: SandboxInput = serde_json::from_str(
            r#"{"code":"print('hi')","language":"python","argv":["--verbose"],"stdin":"data"}"#,
        )
        .unwrap();
        assert_eq!(input.code, "print('hi')");
        assert_eq!(input.language, "python");
        assert_eq!(input.argv, vec!["--verbose"]);
        assert_eq!(input.stdin, "data");
    }

    #[test]
    fn input_argv_defaults_to_empty_vec() {
        let input: SandboxInput = serde_json::from_str(r#"{"code":"x","language":"py"}"#).unwrap();
        assert!(input.argv.is_empty());
    }

    #[test]
    fn input_stdin_defaults_to_empty() {
        let input: SandboxInput = serde_json::from_str(r#"{"code":"x","language":"py"}"#).unwrap();
        assert!(input.stdin.is_empty());
    }

    #[test]
    fn output_serialization() {
        let output = SandboxOutput {
            stdout: "hello\n".into(),
            stderr: String::new(),
            exit_code: 0,
        };
        let json: serde_json::Value = serde_json::to_value(&output).unwrap();
        assert_eq!(json["stdout"], "hello\n");
        assert_eq!(json["stderr"], "");
        assert_eq!(json["exit_code"], 0);
    }

    #[test]
    fn output_nonzero_exit() {
        let output = SandboxOutput {
            stdout: String::new(),
            stderr: "runtime error".into(),
            exit_code: 1,
        };
        let json: serde_json::Value = serde_json::to_value(&output).unwrap();
        assert_eq!(json["exit_code"], 1);
        assert_eq!(json["stderr"], "runtime error");
    }
}
