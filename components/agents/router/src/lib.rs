use agent_types::{TaskInput, TaskOutput};
use extism_pdk::*;

#[derive(serde::Deserialize)]
struct RouterInput {
    #[serde(default)]
    message: String,
    #[serde(default)]
    history: String,
}

#[derive(serde::Serialize, serde::Deserialize)]
struct RouterOutput {
    skill: String,
    params: RouterParams,
}

#[derive(serde::Serialize, serde::Deserialize, Default)]
struct RouterParams {
    #[serde(default, skip_serializing_if = "String::is_empty")]
    query: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    command: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    content: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    op: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    code: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    language: String,
}

#[derive(serde::Serialize)]
struct LlmRequest {
    model: String,
    messages: Vec<LlmMessage>,
    max_tokens: u32,
    temperature: Option<f32>,
}

#[derive(serde::Serialize)]
struct LlmMessage {
    role: String,
    content: String,
}

#[derive(serde::Deserialize)]
#[allow(dead_code)]
struct LlmResponse {
    content: String,
    model_used: String,
}

#[host_fn]
extern "ExtismHost" {
    fn llm_complete(input: Json<LlmRequest>) -> Json<LlmResponse>;
}

const SYSTEM_PROMPT: &str = r#"You are a routing assistant. Given a user message, determine which skill to invoke and extract the parameters. Respond with ONLY a valid JSON object, no markdown fences, no explanation.

Available skills:
- "web-search": search the web. Required params: {"query": "search terms"}
- "shell": run a shell command on the host. Required params: {"command": "ls -la /tmp"}
- "sandbox-exec": run Python code in a sandboxed WASM environment. Required params: {"language": "python", "code": "...source code..."}
- "file-ops": read or write a file. Required params: {"op": "read|write", "path": "/tmp/file.txt"} (add "content" for write)
- "direct-answer": answer directly without a skill. Params: {}

Prefer "sandbox-exec" over "shell" when the user asks to run code, compute something, or execute a script. Use "shell" only for host commands like ls, find, date.

Examples:
User: "search for WebAssembly news" → {"skill":"web-search","params":{"query":"WebAssembly news"}}
User: "run ls /tmp" → {"skill":"shell","params":{"command":"ls /tmp"}}
User: "what is 2+2?" → {"skill":"sandbox-exec","params":{"language":"python","code":"print(2+2)"}}
User: "calculate fibonacci of 10" → {"skill":"sandbox-exec","params":{"language":"python","code":"def fib(n):\n  a,b=0,1\n  for _ in range(n): a,b=b,a+b\n  return a\nprint(fib(10))"}}
User: "read file /tmp/notes.txt" → {"skill":"file-ops","params":{"op":"read","path":"/tmp/notes.txt"}}
User: "write hello to /tmp/wasmclaw/test.txt" → {"skill":"file-ops","params":{"op":"write","path":"/tmp/wasmclaw/test.txt","content":"hello"}}
User: "remember my name is Alice" → {"skill":"direct-answer","params":{}}
"#;

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: RouterInput = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    let user_content = if req.history.is_empty() {
        req.message.clone()
    } else {
        format!(
            "Conversation history:\n{}\n\nCurrent message: {}",
            req.history, req.message
        )
    };

    let llm_req = LlmRequest {
        model: String::new(),
        messages: vec![
            LlmMessage {
                role: "system".to_string(),
                content: SYSTEM_PROMPT.to_string(),
            },
            LlmMessage {
                role: "user".to_string(),
                content: user_content,
            },
        ],
        max_tokens: 256,
        temperature: Some(0.0),
    };

    let Json(llm_resp) = unsafe {
        llm_complete(Json(llm_req)).map_err(|e| Error::msg(format!("LLM error: {e}")))?
    };

    // Parse LLM JSON output; fall back to direct-answer on any parse error.
    let route: RouterOutput = serde_json::from_str(llm_resp.content.trim()).unwrap_or_else(|_| {
        RouterOutput {
            skill: "direct-answer".to_string(),
            params: RouterParams::default(),
        }
    });

    let payload = serde_json::to_string(&route)
        .map_err(|e| Error::msg(format!("serialization error: {e}")))?;

    Ok(Json(TaskOutput {
        payload,
        metadata: vec![],
    }))
}
