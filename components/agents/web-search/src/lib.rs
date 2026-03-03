wit_bindgen::generate!({
    world: "agent",
    path: "../../../wit/agent.wit",
});

use waki::Client;
use wasm_af::agent::host_config::get_config;

#[derive(serde::Deserialize)]
struct SearchRequest {
    query: String,
    #[serde(default)]
    count: Option<u32>,
}

#[derive(serde::Serialize)]
struct SearchOutput {
    query: String,
    results: Vec<SearchResult>,
}

#[derive(serde::Serialize, serde::Deserialize)]
struct SearchResult {
    title: String,
    url: String,
    snippet: String,
}

#[derive(serde::Deserialize)]
struct BraveResponse {
    web: Option<BraveWeb>,
}

#[derive(serde::Deserialize)]
struct BraveWeb {
    results: Vec<BraveResult>,
}

#[derive(serde::Deserialize)]
struct BraveResult {
    title: String,
    url: String,
    description: Option<String>,
}

struct WebSearchAgent;

impl Guest for WebSearchAgent {
    fn execute(input: TaskInput) -> Result<TaskOutput, String> {
        let req: SearchRequest = serde_json::from_str(&input.payload)
            .map_err(|e| format!("payload parse error: {e}"))?;

        if req.query.trim().is_empty() {
            return Err("query field is required and must not be empty".into());
        }

        let mock_mode = get_config("mock_results")
            .map(|v| v.trim().eq_ignore_ascii_case("true"))
            .unwrap_or(false);

        let results = if mock_mode {
            mock_results(&req.query)
        } else {
            let api_key = get_config("brave_api_key")
                .ok_or_else(|| "brave_api_key not set in config".to_string())?;
            brave_search(&req.query, req.count.unwrap_or(5), &api_key)?
        };

        let payload = serde_json::to_string(&SearchOutput {
            query: req.query,
            results,
        })
        .map_err(|e| format!("serialization error: {e}"))?;

        Ok(TaskOutput {
            payload,
            metadata: vec![],
        })
    }
}

export!(WebSearchAgent);

fn brave_search(query: &str, count: u32, api_key: &str) -> Result<Vec<SearchResult>, String> {
    let url = format!(
        "https://api.search.brave.com/res/v1/web/search?q={}&count={count}",
        percent_encode(query),
    );

    let resp = Client::new()
        .get(&url)
        .header("X-Subscription-Token", api_key)
        .header("Accept", "application/json")
        .send()
        .map_err(|e| format!("search request failed: {e}"))?;

    let status = resp.status_code();
    if !(200..300).contains(&status) {
        return Err(format!("Brave Search returned HTTP {status}"));
    }

    let body = resp.body().map_err(|e| format!("read body: {e}"))?;
    let brave: BraveResponse =
        serde_json::from_slice(&body).map_err(|e| format!("JSON parse error: {e}"))?;

    Ok(brave
        .web
        .unwrap_or(BraveWeb { results: vec![] })
        .results
        .into_iter()
        .map(|r| SearchResult {
            title: r.title,
            url: r.url,
            snippet: r.description.unwrap_or_default(),
        })
        .collect())
}

fn mock_results(query: &str) -> Vec<SearchResult> {
    vec![
        SearchResult {
            title: format!("Mock result 1 for: {query}"),
            url: "https://example.com/result-1".to_string(),
            snippet: "This is a mock search result for local development.".to_string(),
        },
        SearchResult {
            title: format!("Mock result 2 for: {query}"),
            url: "https://example.com/result-2".to_string(),
            snippet: "Another mock result. The summarizer will produce a summary of these."
                .to_string(),
        },
        SearchResult {
            title: "wasm-af documentation".to_string(),
            url: "https://github.com/jolucas/wasm-af".to_string(),
            snippet: "WebAssembly agent framework. Policy-gated, orchestrated WASM agents."
                .to_string(),
        },
    ]
}

fn percent_encode(s: &str) -> String {
    let mut out = String::with_capacity(s.len() * 3);
    for byte in s.bytes() {
        match byte {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'-' | b'_' | b'.' | b'~' => {
                out.push(byte as char)
            }
            b' ' => out.push('+'),
            b => out.push_str(&format!("%{b:02X}")),
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn percent_encode_alphanumeric() {
        assert_eq!(percent_encode("hello123"), "hello123");
    }

    #[test]
    fn percent_encode_spaces() {
        assert_eq!(percent_encode("hello world"), "hello+world");
    }

    #[test]
    fn percent_encode_special_chars() {
        assert_eq!(percent_encode("a&b=c"), "a%26b%3Dc");
    }

    #[test]
    fn percent_encode_unreserved() {
        assert_eq!(percent_encode("test-case_v1.0~beta"), "test-case_v1.0~beta");
    }

    #[test]
    fn percent_encode_empty() {
        assert_eq!(percent_encode(""), "");
    }

    #[test]
    fn percent_encode_unicode() {
        let result = percent_encode("日本");
        assert!(!result.contains("日"));
        assert!(result.starts_with('%'));
    }

    #[test]
    fn mock_results_returns_three() {
        let results = mock_results("test query");
        assert_eq!(results.len(), 3);
        assert!(results[0].title.contains("test query"));
    }

    #[test]
    fn mock_results_has_urls() {
        let results = mock_results("anything");
        for r in &results {
            assert!(r.url.starts_with("https://"));
        }
    }
}
