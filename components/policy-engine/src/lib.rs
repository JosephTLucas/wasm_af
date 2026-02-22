wit_bindgen::generate!({
    path: "wit",
    world: "policy-engine",
    generate_all,
});

use exports::wasm_af::policy::evaluator::{
    Capability, CommsMode, DenyCode, DenyReason, Guest, LinkRequest, Permit,
};

use serde::Deserialize;

/// JSON structure of the policy rules config value (key: "policy-rules").
///
/// Example:
/// ```json
/// {
///   "rules": [
///     { "source": "web-search", "target": "summarizer",    "capability": "agent-direct", "comms_mode": "direct"   },
///     { "source": "web-search", "target": "*",             "capability": "http",         "comms_mode": "mediated" },
///     { "source": "summarizer", "target": "*",             "capability": "llm",          "comms_mode": "mediated" }
///   ]
/// }
/// ```
///
/// Pattern matching: `"*"` in source or target matches any string.
/// Rules are evaluated in order; the first match wins.
/// If no rule matches, the request is denied (deny-by-default).
#[derive(Deserialize)]
struct PolicyConfig {
    rules: Vec<Rule>,
}

#[derive(Deserialize)]
struct Rule {
    /// Exact component ID or `"*"` to match any source.
    source: String,
    /// Exact component ID or `"*"` to match any target.
    target: String,
    /// Capability type: "http", "llm", "kv", "agent-direct".
    capability: String,
    /// Communication mode for permitted pairs: "mediated" or "direct".
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

fn capability_str(cap: Capability) -> &'static str {
    match cap {
        Capability::Http => "http",
        Capability::Llm => "llm",
        Capability::Kv => "kv",
        Capability::AgentDirect => "agent-direct",
    }
}

fn comms_mode_from_str(s: &str, context: &str) -> Result<CommsMode, DenyReason> {
    match s {
        "mediated" => Ok(CommsMode::Mediated),
        "direct" => Ok(CommsMode::Direct),
        other => Err(DenyReason {
            code: DenyCode::PolicyConfigError,
            message: format!(
                "rule for {context} has invalid comms_mode '{other}'; expected 'mediated' or 'direct'"
            ),
        }),
    }
}

/// Load and parse the policy config from the WASI runtime config key "policy-rules".
/// Returns a deny-reason error on config read failure or parse failure.
/// Returns deny-all config if the key is unset.
fn load_policy() -> Result<PolicyConfig, DenyReason> {
    let result = wasi::config::runtime::get("policy-rules");

    let raw = result.map_err(|e| DenyReason {
        code: DenyCode::PolicyConfigError,
        message: format!("failed to read 'policy-rules' config: {:?}", e),
    })?;

    let json = raw.unwrap_or_default();

    if json.is_empty() {
        // No config → deny-all (safe default).
        return Err(DenyReason {
            code: DenyCode::PolicyConfigError,
            message: "'policy-rules' config is not set; defaulting to deny-all".to_string(),
        });
    }

    serde_json::from_str::<PolicyConfig>(&json).map_err(|e| DenyReason {
        code: DenyCode::PolicyConfigError,
        message: format!("failed to parse 'policy-rules' JSON: {e}"),
    })
}

struct Component;

impl Guest for Component {
    fn evaluate(req: LinkRequest) -> Result<Permit, DenyReason> {
        let policy = load_policy()?;
        let cap_str = capability_str(req.capability);
        let context = format!(
            "{} -> {} ({})",
            req.source_component_id, req.target_component_id, cap_str
        );

        for rule in &policy.rules {
            if rule.matches(&req.source_component_id, &req.target_component_id, cap_str) {
                let mode = comms_mode_from_str(&rule.comms_mode, &context)?;
                return Ok(Permit { comms_mode: mode });
            }
        }

        Err(DenyReason {
            code: DenyCode::NotAllowed,
            message: format!("no policy rule permits {context}; deny-by-default"),
        })
    }
}

export!(Component);

// --------------------------------------------------------------------------
// Unit tests — exercise pure rule evaluation logic without WASI config I/O.
// --------------------------------------------------------------------------

#[cfg(test)]
fn evaluate_with_policy(policy: &PolicyConfig, req: &LinkRequest) -> Result<Permit, DenyReason> {
    let cap_str = capability_str(req.capability);
    let context = format!(
        "{} -> {} ({})",
        req.source_component_id, req.target_component_id, cap_str
    );
    for rule in &policy.rules {
        if rule.matches(&req.source_component_id, &req.target_component_id, cap_str) {
            let mode = comms_mode_from_str(&rule.comms_mode, &context)?;
            return Ok(Permit { comms_mode: mode });
        }
    }
    Err(DenyReason {
        code: DenyCode::NotAllowed,
        message: format!("no policy rule permits {context}; deny-by-default"),
    })
}

#[cfg(test)]
fn make_policy(rules_json: &str) -> PolicyConfig {
    serde_json::from_str(rules_json).expect("invalid test JSON")
}

#[cfg(test)]
fn req(src: &str, tgt: &str, cap: Capability) -> LinkRequest {
    LinkRequest {
        source_component_id: src.to_string(),
        target_component_id: tgt.to_string(),
        capability: cap,
        task_id: "test-task-1".to_string(),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn exact_match_permitted_mediated() {
        let policy = make_policy(r#"{"rules":[
            {"source":"agent-a","target":"http-provider","capability":"http","comms_mode":"mediated"}
        ]}"#);
        let result = evaluate_with_policy(&policy, &req("agent-a", "http-provider", Capability::Http));
        assert!(result.is_ok());
        assert_eq!(result.unwrap().comms_mode, CommsMode::Mediated);
    }

    #[test]
    fn exact_match_permitted_direct() {
        let policy = make_policy(r#"{"rules":[
            {"source":"web-search","target":"summarizer","capability":"agent-direct","comms_mode":"direct"}
        ]}"#);
        let result = evaluate_with_policy(&policy, &req("web-search", "summarizer", Capability::AgentDirect));
        assert!(result.is_ok());
        assert_eq!(result.unwrap().comms_mode, CommsMode::Direct);
    }

    #[test]
    fn wildcard_source_matches_any() {
        let policy = make_policy(r#"{"rules":[
            {"source":"*","target":"llm-provider","capability":"llm","comms_mode":"mediated"}
        ]}"#);
        let result = evaluate_with_policy(&policy, &req("any-agent-id", "llm-provider", Capability::Llm));
        assert!(result.is_ok());
    }

    #[test]
    fn wildcard_target_matches_any() {
        let policy = make_policy(r#"{"rules":[
            {"source":"agent-a","target":"*","capability":"kv","comms_mode":"mediated"}
        ]}"#);
        let result = evaluate_with_policy(&policy, &req("agent-a", "kv-provider-123", Capability::Kv));
        assert!(result.is_ok());
    }

    #[test]
    fn no_matching_rule_is_denied() {
        let policy = make_policy(r#"{"rules":[
            {"source":"agent-a","target":"http-provider","capability":"http","comms_mode":"mediated"}
        ]}"#);
        let result = evaluate_with_policy(&policy, &req("agent-b", "http-provider", Capability::Http));
        assert!(result.is_err());
        assert_eq!(result.unwrap_err().code, DenyCode::NotAllowed);
    }

    #[test]
    fn wrong_capability_is_denied() {
        let policy = make_policy(r#"{"rules":[
            {"source":"agent-a","target":"http-provider","capability":"http","comms_mode":"mediated"}
        ]}"#);
        let result = evaluate_with_policy(&policy, &req("agent-a", "http-provider", Capability::Llm));
        assert!(result.is_err());
        assert_eq!(result.unwrap_err().code, DenyCode::NotAllowed);
    }

    #[test]
    fn empty_rules_denies_all() {
        let policy = make_policy(r#"{"rules":[]}"#);
        let result = evaluate_with_policy(&policy, &req("agent-a", "anything", Capability::Http));
        assert!(result.is_err());
        assert_eq!(result.unwrap_err().code, DenyCode::NotAllowed);
    }

    #[test]
    fn first_matching_rule_wins() {
        let policy = make_policy(r#"{"rules":[
            {"source":"agent-a","target":"*",          "capability":"http","comms_mode":"direct"},
            {"source":"agent-a","target":"provider-x","capability":"http","comms_mode":"mediated"}
        ]}"#);
        // First rule matches; should be Direct despite second saying Mediated.
        let result = evaluate_with_policy(&policy, &req("agent-a", "provider-x", Capability::Http));
        assert!(result.is_ok());
        assert_eq!(result.unwrap().comms_mode, CommsMode::Direct);
    }

    #[test]
    fn invalid_comms_mode_is_config_error() {
        let policy = make_policy(r#"{"rules":[
            {"source":"agent-a","target":"provider-x","capability":"http","comms_mode":"banana"}
        ]}"#);
        let result = evaluate_with_policy(&policy, &req("agent-a", "provider-x", Capability::Http));
        assert!(result.is_err());
        assert_eq!(result.unwrap_err().code, DenyCode::PolicyConfigError);
    }
}
