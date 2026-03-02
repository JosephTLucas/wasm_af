wit_bindgen::generate!({
    world: "agent",
    path: "../../../wit/agent.wit",
});

use wasm_af::agent::host_llm::{llm_complete, LlmMessage, LlmRequest};

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
    #[serde(default, skip_serializing_if = "String::is_empty")]
    to: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    subject: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    body: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    folder: String,
}

const SYSTEM_PROMPT: &str = r#"/no_think
You are a routing assistant. Given a user message, determine which skill to invoke and extract the parameters. Respond with ONLY a valid JSON object, no markdown fences, no explanation, no thinking.

You MUST select the most specific skill for each request. Only use "direct-answer" for pure knowledge questions that involve no files, commands, code, emails, or web searches.

Available skills:
- "web-search": search the web. Required params: {"query": "search terms"}
- "shell": run a shell command on the host. Required params: {"command": "ls -la /tmp"}
- "sandbox-exec": run Python code in a sandboxed WASM environment. Required params: {"language": "python", "code": "...source code..."}
- "file-ops": read or write a file. Required params: {"op": "read|write", "path": "/tmp/file.txt"} (add "content" for write)
- "email-send": send an email. Required params: {"to": "recipient@example.com", "subject": "Subject line", "body": "Email body text"}
- "email-read": read/check email inbox. Required params: {"folder": "inbox"}
- "direct-answer": answer directly without a skill. Params: {}

Prefer "sandbox-exec" over "shell" when the user asks to run code, compute something, or execute a script. Use "shell" only for host commands like ls, find, date.

Examples:
User: "search for WebAssembly news" → {"skill":"web-search","params":{"query":"WebAssembly news"}}
User: "run ls /tmp" → {"skill":"shell","params":{"command":"ls /tmp"}}
User: "what is 2+2?" → {"skill":"sandbox-exec","params":{"language":"python","code":"print(2+2)"}}
User: "calculate fibonacci of 10" → {"skill":"sandbox-exec","params":{"language":"python","code":"def fib(n):\n  a,b=0,1\n  for _ in range(n): a,b=b,a+b\n  return a\nprint(fib(10))"}}
User: "read file /tmp/notes.txt" → {"skill":"file-ops","params":{"op":"read","path":"/tmp/notes.txt"}}
User: "write hello to /tmp/wasmclaw/test.txt" → {"skill":"file-ops","params":{"op":"write","path":"/tmp/wasmclaw/test.txt","content":"hello"}}
User: "send an email to bob@example.com saying hello" → {"skill":"email-send","params":{"to":"bob@example.com","subject":"hello","body":"hello"}}
User: "check my email" → {"skill":"email-read","params":{"folder":"inbox"}}
User: "remember my name is Alice" → {"skill":"direct-answer","params":{}}
"#;

fn strip_llm_wrapper(raw: &str) -> &str {
    let after_think = match raw.rfind("</think>") {
        Some(idx) => raw[idx + "</think>".len()..].trim(),
        None => raw,
    };
    after_think
        .strip_prefix("```json")
        .or_else(|| after_think.strip_prefix("```"))
        .and_then(|s| s.strip_suffix("```"))
        .map(|s| s.trim())
        .unwrap_or(after_think)
}

struct RouterAgent;

impl Guest for RouterAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: RouterInput = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        if req.message.trim().is_empty() {
            return Err("message field is required and must not be empty".to_string());
        }

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

        let llm_resp = llm_complete(&llm_req).map_err(|e| format!("LLM error: {e}"))?;

        let json_str = strip_llm_wrapper(llm_resp.content.trim());

        let route: RouterOutput = serde_json::from_str(json_str).unwrap_or_else(|_| RouterOutput {
            skill: "direct-answer".to_string(),
            params: RouterParams::default(),
        });

        let payload =
            serde_json::to_string(&route).map_err(|e| format!("serialization error: {e}"))?;

        Ok(TaskOutput {
            payload,
            metadata: vec![],
        })
    }
}

export!(RouterAgent);

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn strip_plain_json() {
        let input = r#"{"skill":"web-search","params":{"query":"test"}}"#;
        assert_eq!(strip_llm_wrapper(input), input);
    }

    #[test]
    fn strip_think_tags() {
        let input = "<think>reasoning here</think>{\"skill\":\"shell\"}";
        assert_eq!(strip_llm_wrapper(input), "{\"skill\":\"shell\"}");
    }

    #[test]
    fn strip_think_tags_with_whitespace() {
        let input = "<think>reasoning\nmultiline</think>  \n  {\"skill\":\"shell\"}  ";
        assert_eq!(strip_llm_wrapper(input), "{\"skill\":\"shell\"}");
    }

    #[test]
    fn strip_markdown_json_fences() {
        let input = "```json\n{\"skill\":\"web-search\"}\n```";
        assert_eq!(strip_llm_wrapper(input), "{\"skill\":\"web-search\"}");
    }

    #[test]
    fn strip_markdown_plain_fences() {
        let input = "```\n{\"skill\":\"shell\"}\n```";
        assert_eq!(strip_llm_wrapper(input), "{\"skill\":\"shell\"}");
    }

    #[test]
    fn strip_think_plus_fences() {
        let input = "<think>some thought</think>\n```json\n{\"skill\":\"web-search\"}\n```";
        assert_eq!(strip_llm_wrapper(input), "{\"skill\":\"web-search\"}");
    }

    #[test]
    fn strip_preserves_inner_content() {
        let input = "```json\n{\"skill\":\"shell\",\"params\":{\"command\":\"echo ```\"}}\n```";
        let result = strip_llm_wrapper(input);
        assert!(result.contains("echo"));
    }

    #[test]
    fn strip_no_fences_returns_original() {
        let input = "just some text without any json";
        assert_eq!(strip_llm_wrapper(input), input);
    }

    #[test]
    fn strip_only_closing_think_takes_last() {
        let input = "ignored prefix </think> {\"skill\":\"shell\"}";
        assert_eq!(strip_llm_wrapper(input), "{\"skill\":\"shell\"}");
    }

    #[test]
    fn strip_empty_input() {
        assert_eq!(strip_llm_wrapper(""), "");
    }
}
