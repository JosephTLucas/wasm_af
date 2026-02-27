use agent_types::{TaskInput, TaskOutput};
use extism_pdk::*;
use std::fs;
use std::path::Path;

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

// No host functions — file I/O goes through the WASI filesystem interface.
// The orchestrator mounts the allowed base paths via the Extism manifest's
// AllowedPaths field, which Wazero enforces at the runtime level.

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: FileOpsInput = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    let output = match req.op.as_str() {
        "read" => {
            if req.path.is_empty() {
                return Err(Error::msg("path is required for read op").into());
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
                return Err(Error::msg("path is required for write op").into());
            }
            if let Some(parent) = Path::new(&req.path).parent() {
                if let Err(e) = fs::create_dir_all(parent) {
                    return Ok(Json(TaskOutput {
                        payload: serde_json::to_string(&FileOpsOutput {
                            content: String::new(),
                            success: false,
                            error: format!("mkdir: {e}"),
                        })
                        .unwrap_or_default(),
                        metadata: vec![],
                    }));
                }
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

    let payload = serde_json::to_string(&output)
        .map_err(|e| Error::msg(format!("serialization error: {e}")))?;

    Ok(Json(TaskOutput {
        payload,
        metadata: vec![],
    }))
}
