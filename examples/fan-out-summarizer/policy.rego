package wasm_af.authz

import rego.v1

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
max_memory_pages := 64 if input.step.agent_type == "url-fetch"
max_memory_pages := 256 if input.step.agent_type == "summarizer"

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

allowed_hosts := ["api.search.brave.com"] if {
	input.step.agent_type == "web-search"
}

# Policy-driven config injection: secrets and feature flags flow from OPA
# data into the plugin's Extism config — never in the task request.
# In production, replace mock_results with the real key from data.secrets.
config := {"brave_api_key": data.secrets.brave_api_key} if {
	input.step.agent_type == "web-search"
	data.secrets.brave_api_key
}

config := {"mock_results": "true"} if {
	input.step.agent_type == "web-search"
	not data.secrets.brave_api_key
}

deny_message := msg if {
	not allow
	msg := sprintf("no rule permits %s (%s); deny-by-default", [input.step.agent_type, input.agent.capability])
}
