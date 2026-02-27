package wasm_af.authz

import rego.v1

default allow := false

# Always-allowed agents (no sensitive capabilities).
allow if {
	input.step.agent_type in {"memory", "router", "responder"}
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

# Router splice validation: the proposed skill must be in the allowed_skills list.
allow if {
	input.step.agent_type == "router-splice"
	input.step.params.proposed_skill in data.config.allowed_skills
}

# Inject per-agent config overrides into the plugin manifest.

# Shell: pass allowed_commands so the host fn can validate defense-in-depth.
config["allowed_commands"] := concat(",", data.config.allowed_commands) if {
	input.step.agent_type == "shell"
}

# File ops: mount each allowed base path into the WASM sandbox (host path → guest path).
# Wazero enforces the boundary at the runtime level — no host function needed.
allowed_paths[base] := base if {
	input.step.agent_type == "file-ops"
	data.config.file_ops_enabled
	some base in data.config.allowed_paths
}
