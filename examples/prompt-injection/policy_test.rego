package wasm_af.authz

import rego.v1

test_url_fetch_localhost_allowed if {
	allow with input as {
		"step": {"agent_type": "url-fetch", "domain": "localhost"},
		"agent": {"capability": "http"},
	}
		with data.config.allowed_domains as ["localhost"]
}

test_url_fetch_evil_denied if {
	not allow with input as {
		"step": {"agent_type": "url-fetch", "domain": "evil.com"},
		"agent": {"capability": "http"},
	}
		with data.config.allowed_domains as ["localhost"]
}

test_summarizer_allowed if {
	allow with input as {
		"step": {"agent_type": "summarizer"},
		"agent": {"capability": "llm"},
	}
}

test_web_search_denied if {
	not allow with input as {
		"step": {"agent_type": "web-search"},
		"agent": {"capability": "http"},
	}
		with data.config.allowed_domains as ["localhost"]
}
