package wasm_af.authz

import rego.v1

default allow := false

# Always-allowed agents (no sensitive capabilities).
allow if {
	input.step.agent_type in {"memory", "router"}
}

# Responder: always allowed unless an email-reply task has injected content
# in the target email. The jailbreak check uses prior_results (the email-read
# output) and the task context (reply_to_index) to inspect only the email
# the agent is about to process.
allow if {
	input.step.agent_type == "responder"
	not email_reply_jailbreak
}

# ── Email-reply jailbreak gate ───────────────────────────────────────────
# Reads reply_to_index from step params first (for parallel reply-all
# branches where each step has its own index), falling back to task context
# (for single email-reply tasks).

_reply_to_index := to_number(input.step.params.reply_to_index) if {
	input.step.params.reply_to_index
}

_reply_to_index := to_number(input.task.context.reply_to_index) if {
	not input.step.params.reply_to_index
	input.task.context.reply_to_index
}

email_reply_jailbreak if {
	input.task.type in {"email-reply", "reply-all"}
	email_output := json.unmarshal(input.prior_results.skill_output)
	email := email_output.emails[_reply_to_index]
	some pattern in data.config.jailbreak_patterns
	contains(lower(email.body), pattern)
}

deny_message := "jailbreak detected in email content — responder blocked" if {
	email_reply_jailbreak
}

deny_message := msg if {
	not allow
	not email_reply_jailbreak
	msg := sprintf("no rule permits %s (%s); deny-by-default", [input.step.agent_type, input.agent.capability])
}

# Web search: requires web_search_enabled flag.
allow if {
	input.step.agent_type == "web-search"
	data.config.web_search_enabled
}

# Shell: binary allowlist + metacharacter rejection + path confinement.
# The orchestrator uses exec.Command (not /bin/sh -c) so metacharacters are
# harmless literals at runtime; blocking them here is defense-in-depth against
# regressions. Path confinement reuses the same allowed_paths as file-ops.
allow if {
	input.step.agent_type == "shell"
	data.config.shell_enabled
	parts := split(input.step.params.command, " ")
	count(parts) > 0
	parts[0] in data.config.allowed_commands
	not command_has_metachar(input.step.params.command)
	not shell_path_violation(parts)
}

# ── Shell helpers ────────────────────────────────────────────────────────────

shell_metachars := {";", "|", "&", "`", "$(", ">", "<"}

command_has_metachar(cmd) if {
	some mc in shell_metachars
	contains(cmd, mc)
}

# Any argument containing ".." is a traversal attempt.
shell_path_violation(parts) if {
	some i
	i > 0
	contains(parts[i], "..")
}

# Any absolute-path argument must be under an allowed base.
shell_path_violation(parts) if {
	some i
	i > 0
	startswith(parts[i], "/")
	not shell_path_allowed(parts[i])
}

shell_path_allowed(p) if {
	some base in data.config.allowed_paths
	p == base
}

shell_path_allowed(p) if {
	some base in data.config.allowed_paths
	startswith(p, concat("", [base, "/"]))
}

# Sandbox exec: code runs inside WASM (Wazero), not on the host.
# Policy can be permissive — arbitrary code is safe because it cannot escape
# the Wazero sandbox. Only the language must be in the allowlist.
allow if {
	input.step.agent_type == "sandbox-exec"
	data.config.sandbox_exec_enabled
	input.step.params.language in data.config.allowed_languages
}

# File ops: path must be under an allowed base path.
# Use a path-component boundary check (base + "/") to prevent prefix-escape
# attacks where /tmp/wasmclaw-escape would otherwise match /tmp/wasmclaw.
allow if {
	input.step.agent_type == "file-ops"
	data.config.file_ops_enabled
	some base in data.config.allowed_paths
	startswith(input.step.params.path, concat("", [base, "/"]))
}

# Email send: host function mediates delivery; SMTP creds live in Go closure.
# No secrets enter WASM — the agent only sees success/failure from the host fn.
allow if {
	input.step.agent_type == "email-send"
	data.config.email_send_enabled
}

# Email read: sandboxed agent with OPA-injected API key (see config rules below).
# Has zero host functions and zero network capability — structurally cannot
# exfiltrate the key even if email content contains prompt injection.
allow if {
	input.step.agent_type == "email-read"
	data.config.email_read_enabled
}

# Router splice validation: the proposed skill must be in the allowed_skills list.
allow if {
	input.step.agent_type == "router-splice"
	input.step.params.proposed_skill in data.config.allowed_skills
}

# ── Human-in-the-loop approval gates ─────────────────────────────────────
# Steps that are allowed by policy but require human confirmation before
# the plugin is created. The orchestrator pauses the step and publishes
# an approval event; execution resumes only after an explicit approve call.

default requires_approval := false

requires_approval if {
	data.config.approval_enabled
	input.step.agent_type == "email-send"
}

requires_approval if {
	data.config.approval_enabled
	input.step.agent_type == "shell"
	input.step.params.command != ""
	parts := split(input.step.params.command, " ")
	not parts[0] in data.config.auto_approved_commands
}

approval_reason := "email delivery requires human approval" if {
	data.config.approval_enabled
	input.step.agent_type == "email-send"
}

approval_reason := sprintf("shell command '%s' requires approval", [input.step.params.command]) if {
	data.config.approval_enabled
	input.step.agent_type == "shell"
	input.step.params.command != ""
	parts := split(input.step.params.command, " ")
	not parts[0] in data.config.auto_approved_commands
}

# ── Per-agent config overrides ───────────────────────────────────────────
# Inject per-agent config overrides into the plugin manifest.

# Shell: pass allowed_commands so the host fn can validate defense-in-depth.
config["allowed_commands"] := concat(",", data.config.allowed_commands) if {
	input.step.agent_type == "shell"
}

# Email read: inject email_api_key from secrets into plugin config.
# The key flows: data.json → OPA → plugin manifest → WASM config API.
# It never appears in task payloads or agent-to-agent communication.
config["email_api_key"] := data.secrets.email_api_key if {
	input.step.agent_type == "email-read"
	data.secrets.email_api_key
}

# Email read fallback: inject a mock key when no real secret is configured,
# so the agent can still run in demo/test mode.
config["email_api_key"] := "mock-email-api-key-DO-NOT-LEAK" if {
	input.step.agent_type == "email-read"
	not data.secrets.email_api_key
}

# File ops: mount each allowed base path into the WASM sandbox (host path → guest path).
# Wazero enforces the boundary at the runtime level — no host function needed.
allowed_paths[base] := base if {
	input.step.agent_type == "file-ops"
	data.config.file_ops_enabled
	some base in data.config.allowed_paths
}
