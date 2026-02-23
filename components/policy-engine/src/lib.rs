use extism_pdk::*;

#[derive(serde::Deserialize)]
struct PolicyConfig {
    rules: Vec<Rule>,
}

#[derive(serde::Deserialize)]
struct Rule {
    source: String,
    target: String,
    capability: String,
    comms_mode: String,
}

impl Rule {
    fn matches(&self, source: &str, target: &str, cap: &str) -> bool {
        let src_ok = self.source == "*" || self.source == source;
        let tgt_ok = self.target == "*" || self.target == target;
        let cap_ok = self.capability == cap;
        src_ok && tgt_ok && cap_ok
    }
}

#[derive(serde::Deserialize)]
#[allow(dead_code)]
struct PolicyRequest {
    source: String,
    target: String,
    capability: String,
    task_id: String,
}

#[derive(serde::Serialize)]
struct PolicyResult {
    permitted: bool,
    comms_mode: Option<String>,
    deny_code: Option<String>,
    deny_message: Option<String>,
}

#[plugin_fn]
pub fn evaluate(Json(req): Json<PolicyRequest>) -> FnResult<Json<PolicyResult>> {
    let json = config::get("policy-rules").unwrap_or(None).unwrap_or_default();

    if json.is_empty() {
        return Ok(Json(PolicyResult {
            permitted: false,
            deny_code: Some("policy-config-error".to_string()),
            deny_message: Some("'policy-rules' config is not set; defaulting to deny-all".to_string()),
            comms_mode: None,
        }));
    }

    let policy: PolicyConfig = serde_json::from_str(&json).map_err(|e| {
        Error::msg(format!("failed to parse 'policy-rules' JSON: {e}"))
    })?;

    let context = format!("{} -> {} ({})", req.source, req.target, req.capability);

    for rule in &policy.rules {
        if rule.matches(&req.source, &req.target, &req.capability) {
            if rule.comms_mode != "mediated" && rule.comms_mode != "direct" {
                return Ok(Json(PolicyResult {
                    permitted: false,
                    deny_code: Some("policy-config-error".to_string()),
                    deny_message: Some(format!(
                        "rule for {context} has invalid comms_mode '{}'; \
                         expected 'mediated' or 'direct'",
                        rule.comms_mode
                    )),
                    comms_mode: None,
                }));
            }
            return Ok(Json(PolicyResult {
                permitted: true,
                comms_mode: Some(rule.comms_mode.clone()),
                deny_code: None,
                deny_message: None,
            }));
        }
    }

    Ok(Json(PolicyResult {
        permitted: false,
        deny_code: Some("not-allowed".to_string()),
        deny_message: Some(format!("no policy rule permits {context}; deny-by-default")),
        comms_mode: None,
    }))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn eval(policy_json: &str, src: &str, tgt: &str, cap: &str) -> PolicyResult {
        let policy: PolicyConfig = serde_json::from_str(policy_json).unwrap();
        let context = format!("{src} -> {tgt} ({cap})");
        for rule in &policy.rules {
            if rule.matches(src, tgt, cap) {
                return PolicyResult {
                    permitted: true,
                    comms_mode: Some(rule.comms_mode.clone()),
                    deny_code: None,
                    deny_message: None,
                };
            }
        }
        PolicyResult {
            permitted: false,
            deny_code: Some("not-allowed".to_string()),
            deny_message: Some(format!("no policy rule permits {context}; deny-by-default")),
            comms_mode: None,
        }
    }

    #[test]
    fn exact_match_permitted() {
        let r = eval(
            r#"{"rules":[{"source":"a","target":"b","capability":"http","comms_mode":"mediated"}]}"#,
            "a", "b", "http",
        );
        assert!(r.permitted);
        assert_eq!(r.comms_mode.as_deref(), Some("mediated"));
    }

    #[test]
    fn wildcard_source() {
        let r = eval(
            r#"{"rules":[{"source":"*","target":"b","capability":"llm","comms_mode":"mediated"}]}"#,
            "any-agent", "b", "llm",
        );
        assert!(r.permitted);
    }

    #[test]
    fn no_match_denied() {
        let r = eval(
            r#"{"rules":[{"source":"a","target":"b","capability":"http","comms_mode":"mediated"}]}"#,
            "c", "b", "http",
        );
        assert!(!r.permitted);
        assert_eq!(r.deny_code.as_deref(), Some("not-allowed"));
    }

    #[test]
    fn empty_rules_denied() {
        let r = eval(r#"{"rules":[]}"#, "a", "b", "http");
        assert!(!r.permitted);
    }

    #[test]
    fn first_match_wins() {
        let r = eval(
            r#"{"rules":[
                {"source":"a","target":"*","capability":"http","comms_mode":"direct"},
                {"source":"a","target":"b","capability":"http","comms_mode":"mediated"}
            ]}"#,
            "a", "b", "http",
        );
        assert!(r.permitted);
        assert_eq!(r.comms_mode.as_deref(), Some("direct"));
    }
}
