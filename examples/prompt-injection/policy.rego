package wasm_af.authz

import rego.v1

default allow := false

# Only url-fetch and summarizer are permitted — web-search is deliberately
# excluded to demonstrate that the prompt-injection agent cannot escalate
# its own capabilities.

allow if {
	input.step.agent_type == "url-fetch"
	input.agent.capability == "http"
	input.step.domain in data.config.allowed_domains
}

allow if {
	input.step.agent_type == "summarizer"
	input.agent.capability == "llm"
}

max_memory_pages := 64 if input.step.agent_type == "url-fetch"
max_memory_pages := 256 if input.step.agent_type == "summarizer"

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
