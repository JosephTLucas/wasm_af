package wasm_af.authz

import rego.v1

# ── BYOA (Bring Your Own Agent) policy tier ──────────────────────────────
# External agents are registered at runtime via POST /agents and always
# receive capability == "untrusted". These rules enforce a maximally
# restrictive sandbox that can be relaxed per-agent through OPA data:
#
#   data.config.approved_external_agents  — set of wasm_names allowed to run
#   data.config.byoa_max_memory_pages     — optional memory override (default 64 = 4 MiB)
#   data.config.byoa_timeout_sec          — optional timeout override (default 10)
#
# Drop this file alongside your existing policy.rego. Because both files
# share the same Rego package, the rules merge automatically.

# Gate: untrusted agents must appear in the approved list.
allow if {
	input.agent.capability == "untrusted"
	input.agent.wasm_name in data.config.approved_external_agents
}

# All untrusted agents require human approval before execution.
requires_approval if {
	input.agent.capability == "untrusted"
}

approval_reason := sprintf("external agent %q requires human approval", [input.agent.wasm_name]) if {
	input.agent.capability == "untrusted"
}

# ── Resource caps ────────────────────────────────────────────────────────
# No host functions — the agent runs in pure computation mode.
host_functions := [] if {
	input.agent.capability == "untrusted"
}

# Strict memory limit (default 64 pages = 4 MiB).
default _byoa_max_memory_pages := 64

_byoa_max_memory_pages := data.config.byoa_max_memory_pages if {
	data.config.byoa_max_memory_pages
}

max_memory_pages := _byoa_max_memory_pages if {
	input.agent.capability == "untrusted"
}

# Strict timeout (default 10 seconds).
default _byoa_timeout_sec := 10

_byoa_timeout_sec := data.config.byoa_timeout_sec if {
	data.config.byoa_timeout_sec
}

timeout_sec := _byoa_timeout_sec if {
	input.agent.capability == "untrusted"
}

# No network access — empty allowed_hosts blocks all outbound HTTP.
allowed_hosts := [] if {
	input.agent.capability == "untrusted"
}
