package wasm_af.authz

test_url_fetch_allowed_domain if {
	allow with input as {
		"step": {"agent_type": "url-fetch", "domain": "webassembly.org"},
		"agent": {"capability": "http"},
	}
		with data.config.allowed_domains as ["webassembly.org"]
}

test_url_fetch_blocked_domain if {
	not allow with input as {
		"step": {"agent_type": "url-fetch", "domain": "evil.com"},
		"agent": {"capability": "http"},
	}
		with data.config.allowed_domains as ["webassembly.org"]
}

test_url_fetch_sets_allowed_hosts if {
	allowed_hosts == ["webassembly.org"] with input as {
		"step": {"agent_type": "url-fetch", "domain": "webassembly.org"},
		"agent": {"capability": "http"},
	}
		with data.config.allowed_domains as ["webassembly.org"]
}

test_url_fetch_memory_limit if {
	max_memory_pages == 64 with input as {
		"step": {"agent_type": "url-fetch", "domain": "webassembly.org"},
		"agent": {"capability": "http"},
	}
		with data.config.allowed_domains as ["webassembly.org"]
}

test_summarizer_allowed if {
	allow with input as {
		"step": {"agent_type": "summarizer"},
		"agent": {"capability": "llm"},
	}
}

test_summarizer_memory_limit if {
	max_memory_pages == 256 with input as {
		"step": {"agent_type": "summarizer"},
		"agent": {"capability": "llm"},
	}
}

test_web_search_allowed if {
	allow with input as {
		"step": {"agent_type": "web-search"},
		"agent": {"capability": "http"},
	}
}

test_unknown_agent_denied if {
	not allow with input as {
		"step": {"agent_type": "evil-agent"},
		"agent": {"capability": "http"},
	}
		with data.config.allowed_domains as ["webassembly.org"]
}

test_web_search_config_mock_mode if {
	config == {"mock_results": "true"} with input as {
		"step": {"agent_type": "web-search"},
		"agent": {"capability": "http"},
	}
		with data.secrets as {}
}

test_web_search_config_real_key if {
	config == {"brave_api_key": "BSA-real-key"} with input as {
		"step": {"agent_type": "web-search"},
		"agent": {"capability": "http"},
	}
		with data.secrets.brave_api_key as "BSA-real-key"
}
