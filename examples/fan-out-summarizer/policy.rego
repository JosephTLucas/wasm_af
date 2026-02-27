package wasm_af.authz

default allow := false

# url-fetch: allow only if the target domain is in the data-driven allowlist
allow if {
	input.step.agent_type == "url-fetch"
	input.agent.capability == "http"
	input.step.domain in data.config.allowed_domains
}

# web-search: always allow http
allow if {
	input.step.agent_type == "web-search"
	input.agent.capability == "http"
}

# summarizer: allow llm
allow if {
	input.step.agent_type == "summarizer"
	input.agent.capability == "llm"
}

# Policy-driven resource limits per agent type
max_memory_pages := 64 if { input.step.agent_type == "url-fetch" }
max_memory_pages := 256 if { input.step.agent_type == "summarizer" }

# Policy-driven allowed_hosts: use restricted_to if present (isolation test),
# otherwise restrict to exactly the target domain.
allowed_hosts := [input.step.params.restricted_to] if {
	input.step.agent_type == "url-fetch"
	input.step.params.restricted_to
}

allowed_hosts := [input.step.domain] if {
	input.step.agent_type == "url-fetch"
	input.step.domain
	not input.step.params.restricted_to
}

deny_message := msg if {
	not allow
	msg := sprintf("no rule permits %s (%s); deny-by-default", [input.step.agent_type, input.agent.capability])
}
