wit_bindgen::generate!({
    world: "agent",
    path: "../../../wit/agent.wit",
});

use std::fs;
use std::path::Path;

struct FileOpsAgent;

export!(FileOpsAgent);

#[derive(serde::Deserialize)]
struct FileOpsInput {
    #[serde(default)]
    op: String,
    #[serde(default)]
    path: String,
    #[serde(default)]
    content: String,
}

#[derive(serde::Serialize)]
struct FileOpsOutput {
    content: String,
    success: bool,
    error: String,
}

impl Guest for FileOpsAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: FileOpsInput = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        let output = match req.op.as_str() {
            "read" => {
                if req.path.is_empty() {
                    return Err("path is required for read op".to_string());
                }
                match fs::read_to_string(&req.path) {
                    Ok(content) => FileOpsOutput {
                        content,
                        success: true,
                        error: String::new(),
                    },
                    Err(e) => FileOpsOutput {
                        content: String::new(),
                        success: false,
                        error: e.to_string(),
                    },
                }
            }
            "write" => {
                if req.path.is_empty() {
                    return Err("path is required for write op".to_string());
                }
                if let Some(parent) = Path::new(&req.path).parent() {
                    let _ = fs::create_dir_all(parent);
                }
                match fs::write(&req.path, req.content.as_bytes()) {
                    Ok(()) => FileOpsOutput {
                        content: String::new(),
                        success: true,
                        error: String::new(),
                    },
                    Err(e) => FileOpsOutput {
                        content: String::new(),
                        success: false,
                        error: e.to_string(),
                    },
                }
            }
            op => FileOpsOutput {
                content: String::new(),
                success: false,
                error: format!("unknown op: {op}; expected read or write"),
            },
        };

        let payload =
            serde_json::to_string(&output).map_err(|e| format!("serialization error: {e}"))?;
        Ok(TaskOutput {
            payload,
            metadata: vec![],
        })
    }
}
