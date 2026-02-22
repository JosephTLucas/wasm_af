wit_bindgen::generate!({
    path: "wit",
    world: "summarizer-agent",
    generate_all,
});

use exports::wasm_af::agent::handler::{AgentError, Guest, TaskInput, TaskOutput};
// ErrorCode lives in the imported types interface, not in handler.
use crate::wasm_af::agent::types::ErrorCode;

// LLM inference — import satisfied at runtime by the llm-inference provider link.
use crate::wasm_af::llm::inference::{self as llm, CompletionRequest, Message};

// ---------------------------------------------------------------------------
// Component registration
// ---------------------------------------------------------------------------

struct Component;
export!(Component);

// ---------------------------------------------------------------------------
// Input / output shapes
// ---------------------------------------------------------------------------

/// Input payload for the summarizer. The `query` field is optional; if omitted
/// the agent summarises whatever search results are in the context.
#[derive(serde::Deserialize)]
struct SummarizerInput {
    #[serde(default)]
    query: Option<String>,
    /// Override the model to use. Falls back to provider default when absent.
    #[serde(default)]
    model: Option<String>,
    /// Maximum tokens for the generated summary (default: 512).
    #[serde(default)]
    max_tokens: Option<u32>,
}

/// Search results produced by the web-search agent and stored in context.
#[derive(serde::Deserialize)]
struct SearchOutput {
    query: String,
    results: Vec<SearchResult>,
}

#[derive(serde::Deserialize)]
struct SearchResult {
    title: String,
    url: String,
    snippet: String,
}

#[derive(serde::Serialize)]
struct SummaryOutput {
    summary: String,
    source_query: String,
    source_count: usize,
}

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const CONTEXT_KEY_WEB_SEARCH: &str = "web_search_results";
const DEFAULT_MODEL: &str = "";    // empty → provider picks its configured default
const DEFAULT_MAX_TOKENS: u32 = 512;

// ---------------------------------------------------------------------------
// Agent handler implementation
// ---------------------------------------------------------------------------

impl Guest for Component {
    fn execute(input: TaskInput) -> Result<TaskOutput, AgentError> {
        // 1. Parse the input payload (optional fields).
        let req: SummarizerInput =
            serde_json::from_str(&input.payload).map_err(|e| AgentError {
                code: ErrorCode::InvalidInput,
                message: format!("payload parse error: {e}"),
            })?;

        // 2. Retrieve search results from the task context (written by the web-search step).
        let search_json = input
            .context
            .iter()
            .find(|kv| kv.key == CONTEXT_KEY_WEB_SEARCH)
            .map(|kv| kv.val.as_str())
            .ok_or_else(|| AgentError {
                code: ErrorCode::InvalidInput,
                message: format!(
                    "context key '{CONTEXT_KEY_WEB_SEARCH}' not found; \
                     web-search step must run before summarizer"
                ),
            })?;

        let search: SearchOutput =
            serde_json::from_str(search_json).map_err(|e| AgentError {
                code: ErrorCode::InvalidInput,
                message: format!("web_search_results parse error: {e}"),
            })?;

        if search.results.is_empty() {
            return Ok(TaskOutput {
                payload: serde_json::to_string(&SummaryOutput {
                    summary: "No search results were found for this query.".to_string(),
                    source_query: search.query,
                    source_count: 0,
                })
                .unwrap(),
                metadata: vec![],
            });
        }

        // 3. Build the LLM prompt.
        let query_label = req
            .query
            .as_deref()
            .unwrap_or_else(|| search.query.as_str());

        let source_count = search.results.len();
        let sources_text = search
            .results
            .iter()
            .enumerate()
            .map(|(i, r)| {
                format!(
                    "[{}] {}\n    URL: {}\n    {}",
                    i + 1,
                    r.title,
                    r.url,
                    r.snippet
                )
            })
            .collect::<Vec<_>>()
            .join("\n\n");

        let user_message = format!(
            "Please provide a concise, accurate summary of the following \
             web search results for the query: \"{query_label}\"\n\n\
             Search results:\n{sources_text}\n\n\
             Write a clear, factual summary in 2-4 paragraphs. \
             Cite sources by their bracketed number where relevant."
        );

        // 4. Call the LLM inference provider.
        let llm_req = CompletionRequest {
            model: req.model.unwrap_or_else(|| DEFAULT_MODEL.to_string()),
            messages: vec![
                Message {
                    role: "system".to_string(),
                    content: "You are a research assistant. Summarize web search results \
                              accurately and concisely, citing sources."
                        .to_string(),
                },
                Message {
                    role: "user".to_string(),
                    content: user_message,
                },
            ],
            max_tokens: req.max_tokens.unwrap_or(DEFAULT_MAX_TOKENS),
            temperature: Some(0.3),
        };

        let llm_resp = llm::complete(&llm_req).map_err(|e| AgentError {
            code: ErrorCode::CapabilityFailure,
            message: format!("LLM inference error ({:?}): {}", e.code, e.message),
        })?;

        // 5. Serialize and return the summary.
        let output = SummaryOutput {
            summary: llm_resp.content,
            source_query: search.query,
            source_count,
        };

        let payload = serde_json::to_string(&output).map_err(|e| AgentError {
            code: ErrorCode::Internal,
            message: format!("serialization error: {e}"),
        })?;

        Ok(TaskOutput {
            payload,
            metadata: vec![],
        })
    }
}
