use agent_types::{TaskInput, TaskOutput};
use extism_pdk::*;

#[derive(serde::Deserialize)]
struct FetchRequest {
    url: String,
}

#[derive(serde::Serialize)]
struct FetchOutput {
    query: String,
    results: Vec<FetchResult>,
}

#[derive(serde::Serialize)]
struct FetchResult {
    title: String,
    url: String,
    snippet: String,
}

const SNIPPET_CHARS: usize = 2000;

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: FetchRequest = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    if req.url.is_empty() {
        return Err(Error::msg("url field is required").into());
    }

    let http_req = HttpRequest::new(&req.url)
        .with_header("User-Agent", "wasm-af-url-fetch/0.1")
        .with_header("Accept", "text/html, text/plain, */*");

    let resp = http::request::<Vec<u8>>(&http_req, None)
        .map_err(|e| Error::msg(format!("fetch failed: {e}")))?;

    let status = resp.status_code();
    if !(200..300).contains(&status) {
        return Err(Error::msg(format!("HTTP {status}")).into());
    }

    let raw_body = resp.body();
    let body = String::from_utf8_lossy(&raw_body);
    let title = extract_title(&body).unwrap_or_else(|| req.url.clone());
    let snippet = truncate_chars(&body, SNIPPET_CHARS);

    let output = FetchOutput {
        query: req.url.clone(),
        results: vec![FetchResult {
            title,
            url: req.url,
            snippet,
        }],
    };

    let payload = serde_json::to_string(&output)
        .map_err(|e| Error::msg(format!("serialization error: {e}")))?;

    Ok(Json(TaskOutput {
        payload,
        metadata: vec![],
    }))
}

fn extract_title(html: &str) -> Option<String> {
    let lower = html.to_ascii_lowercase();
    let start = lower.find("<title")?.checked_add(6)?;
    let after_tag = lower[start..].find('>')?.checked_add(1)?;
    let content_start = start + after_tag;
    let end = lower[content_start..].find("</title>")?;
    let title = html[content_start..content_start + end].trim();
    if title.is_empty() {
        None
    } else {
        Some(title.to_string())
    }
}

fn truncate_chars(s: &str, max: usize) -> String {
    if s.chars().count() <= max {
        s.to_string()
    } else {
        let truncated: String = s.chars().take(max).collect();
        format!("{truncated}…")
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn extract_title_basic() {
        let html = "<html><head><title>Hello World</title></head></html>";
        assert_eq!(extract_title(html), Some("Hello World".to_string()));
    }

    #[test]
    fn extract_title_with_attributes() {
        let html = r#"<html><title lang="en">Page Title</title></html>"#;
        assert_eq!(extract_title(html), Some("Page Title".to_string()));
    }

    #[test]
    fn extract_title_case_insensitive() {
        let html = "<HTML><TITLE>Upper Case</TITLE></HTML>";
        assert_eq!(extract_title(html), Some("Upper Case".to_string()));
    }

    #[test]
    fn extract_title_with_whitespace() {
        let html = "<title>  Trimmed  </title>";
        assert_eq!(extract_title(html), Some("Trimmed".to_string()));
    }

    #[test]
    fn extract_title_empty() {
        let html = "<title></title>";
        assert_eq!(extract_title(html), None);
    }

    #[test]
    fn extract_title_missing() {
        let html = "<html><body>No title here</body></html>";
        assert_eq!(extract_title(html), None);
    }

    #[test]
    fn extract_title_no_closing_tag() {
        let html = "<title>Incomplete";
        assert_eq!(extract_title(html), None);
    }

    #[test]
    fn truncate_within_limit() {
        let s = "hello";
        assert_eq!(truncate_chars(s, 10), "hello");
    }

    #[test]
    fn truncate_at_limit() {
        let s = "12345";
        assert_eq!(truncate_chars(s, 5), "12345");
    }

    #[test]
    fn truncate_over_limit() {
        let s = "hello world";
        let result = truncate_chars(s, 5);
        assert_eq!(result, "hello…");
    }

    #[test]
    fn truncate_unicode() {
        let s = "日本語のテスト";
        let result = truncate_chars(s, 3);
        assert_eq!(result, "日本語…");
    }
}
