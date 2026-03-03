package wasm_af.authz

import rego.v1

default allow := false

# ── Platform: url-fetch (HTTP capability) ────────────────────────────────────
allow if {
	input.step.agent_type == "url-fetch"
	input.agent.capability == "http"
}

allowed_hosts := data.config.allowed_domains if {
	input.step.agent_type == "url-fetch"
}

host_functions := ["http"] if {
	input.step.agent_type == "url-fetch"
}

# ── Platform: responder (LLM capability) ─────────────────────────────────────
allow if {
	input.step.agent_type == "responder"
	input.agent.capability == "llm"
}

host_functions := ["llm_complete"] if {
	input.step.agent_type == "responder"
}

# ── BYOA: untrusted agents ──────────────────────────────────────────────────
# Untrusted agents must appear in the approved list.
allow if {
	input.agent.capability == "untrusted"
	input.agent.wasm_name in data.config.approved_external_agents
}

# All untrusted agents require human approval before execution.
requires_approval if {
	input.agent.capability == "untrusted"
}

approval_reason := sprintf("BYOA agent '%s' requires human approval", [input.agent.wasm_name]) if {
	input.agent.capability == "untrusted"
}

# No host functions for untrusted agents — pure computation only.
host_functions := [] if {
	input.agent.capability == "untrusted"
}

# Memory limit for Python components (256 pages = 16 MiB).
# CPython-in-WASM requires ~200 pages minimum; 256 gives headroom.
max_memory_pages := 256 if {
	input.agent.capability == "untrusted"
}

# Strict timeout (10 seconds).
timeout_sec := 10 if {
	input.agent.capability == "untrusted"
}

# No network access.
allowed_hosts := [] if {
	input.agent.capability == "untrusted"
}

# ── Deny message ─────────────────────────────────────────────────────────────
deny_message := sprintf("agent '%s' (capability: '%s') is not permitted for step '%s'", [
	input.agent.wasm_name,
	input.agent.capability,
	input.step.agent_type,
]) if {
	not allow
}
