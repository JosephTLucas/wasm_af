use agent_types::{TaskInput, TaskOutput};
use extism_pdk::*;

#[derive(serde::Deserialize)]
struct SummarizerInput {
    #[serde(default)]
    query: Option<String>,
    #[serde(default)]
    model: Option<String>,
    #[serde(default)]
    max_tokens: Option<u32>,
}

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

const CONTEXT_KEY_WEB_SEARCH: &str = "web_search_results";
const DEFAULT_MAX_TOKENS: u32 = 512;

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: SummarizerInput = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    let search_json = input
        .context
        .iter()
        .find(|kv| kv.key == CONTEXT_KEY_WEB_SEARCH)
        .map(|kv| kv.val.as_str())
        .ok_or_else(|| {
            Error::msg(format!(
                "context key '{CONTEXT_KEY_WEB_SEARCH}' not found; \
                 web-search step must run before summarizer"
            ))
        })?;

    // Handle both single SearchOutput and array-of-SearchOutput (generic merge).
    let search: SearchOutput = match serde_json::from_str::<SearchOutput>(search_json) {
        Ok(s) => s,
        Err(_) => {
            let items: Vec<SearchOutput> = serde_json::from_str(search_json)
                .map_err(|e| Error::msg(format!("web_search_results parse error: {e}")))?;
            let mut merged = SearchOutput {
                query: String::new(),
                results: Vec::new(),
            };
            let mut queries = Vec::new();
            for item in items {
                queries.push(item.query);
                merged.results.extend(item.results);
            }
            merged.query = queries.join(" | ");
            merged
        }
    };

    if search.results.is_empty() {
        let payload = serde_json::to_string(&SummaryOutput {
            summary: "No search results were found for this query.".to_string(),
            source_query: search.query,
            source_count: 0,
        })?;
        return Ok(Json(TaskOutput {
            payload,
            metadata: vec![],
        }));
    }

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

    let llm_req = LlmRequest {
        model: req.model.unwrap_or_default(),
        messages: vec![
            LlmMessage {
                role: "system".to_string(),
                content: "You are a research assistant. Summarize web search results \
                          accurately and concisely, citing sources."
                    .to_string(),
            },
            LlmMessage {
                role: "user".to_string(),
                content: user_message,
            },
        ],
        max_tokens: req.max_tokens.unwrap_or(DEFAULT_MAX_TOKENS),
        temperature: Some(0.3),
    };

    let Json(llm_resp) = unsafe {
        llm_complete(Json(llm_req))
            .map_err(|e| Error::msg(format!("LLM inference error: {e}")))?
    };

    let output = SummaryOutput {
        summary: llm_resp.content,
        source_query: search.query,
        source_count,
    };

    let payload = serde_json::to_string(&output)
        .map_err(|e| Error::msg(format!("serialization error: {e}")))?;

    Ok(Json(TaskOutput {
        payload,
        metadata: vec![],
    }))
}
