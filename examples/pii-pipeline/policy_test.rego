package wasm_af.authz

import rego.v1

_approved := ["pii-redactor"]

_allowed_domains := ["localhost", "127.0.0.1"]

# ── Platform: url-fetch allowed ──────────────────────────────────────────────

test_url_fetch_allowed if {
	allow with input as {
		"step": {"agent_type": "url-fetch"},
		"agent": {"capability": "http", "wasm_name": "url-fetch"},
	}
}

test_url_fetch_host_functions if {
	host_functions == ["http"] with input as {
		"step": {"agent_type": "url-fetch"},
		"agent": {"capability": "http", "wasm_name": "url-fetch"},
	}
}

test_url_fetch_allowed_hosts if {
	allowed_hosts == _allowed_domains with input as {
		"step": {"agent_type": "url-fetch"},
		"agent": {"capability": "http", "wasm_name": "url-fetch"},
	}
		with data.config.allowed_domains as _allowed_domains
}

# ── Platform: url-fetch denied with wrong capability ─────────────────────────

test_url_fetch_wrong_capability_denied if {
	not allow with input as {
		"step": {"agent_type": "url-fetch"},
		"agent": {"capability": "llm", "wasm_name": "url-fetch"},
	}
}

# ── Platform: responder allowed ──────────────────────────────────────────────

test_responder_allowed if {
	allow with input as {
		"step": {"agent_type": "responder"},
		"agent": {"capability": "llm", "wasm_name": "responder"},
	}
}

test_responder_host_functions if {
	host_functions == ["llm_complete"] with input as {
		"step": {"agent_type": "responder"},
		"agent": {"capability": "llm", "wasm_name": "responder"},
	}
}

# ── Platform: responder denied with wrong capability ─────────────────────────

test_responder_wrong_capability_denied if {
	not allow with input as {
		"step": {"agent_type": "responder"},
		"agent": {"capability": "http", "wasm_name": "responder"},
	}
}

# ── BYOA: approved untrusted agent ───────────────────────────────────────────

test_byoa_approved_agent_allowed if {
	allow with input as {
		"step": {"agent_type": "pii-redactor"},
		"agent": {"capability": "untrusted", "wasm_name": "pii-redactor"},
	}
		with data.config.approved_external_agents as _approved
}

test_byoa_requires_approval if {
	requires_approval with input as {
		"step": {"agent_type": "pii-redactor"},
		"agent": {"capability": "untrusted", "wasm_name": "pii-redactor"},
	}
		with data.config.approved_external_agents as _approved
}

test_byoa_approval_reason_set if {
	approval_reason == "BYOA agent 'pii-redactor' requires human approval" with input as {
		"step": {"agent_type": "pii-redactor"},
		"agent": {"capability": "untrusted", "wasm_name": "pii-redactor"},
	}
		with data.config.approved_external_agents as _approved
}

test_byoa_no_host_functions if {
	host_functions == [] with input as {
		"step": {"agent_type": "pii-redactor"},
		"agent": {"capability": "untrusted", "wasm_name": "pii-redactor"},
	}
		with data.config.approved_external_agents as _approved
}

test_byoa_no_network_access if {
	allowed_hosts == [] with input as {
		"step": {"agent_type": "pii-redactor"},
		"agent": {"capability": "untrusted", "wasm_name": "pii-redactor"},
	}
		with data.config.approved_external_agents as _approved
}

test_byoa_memory_limit if {
	max_memory_pages == 256 with input as {
		"step": {"agent_type": "pii-redactor"},
		"agent": {"capability": "untrusted", "wasm_name": "pii-redactor"},
	}
		with data.config.approved_external_agents as _approved
}

test_byoa_timeout if {
	timeout_sec == 10 with input as {
		"step": {"agent_type": "pii-redactor"},
		"agent": {"capability": "untrusted", "wasm_name": "pii-redactor"},
	}
		with data.config.approved_external_agents as _approved
}

# ── BYOA: unapproved untrusted agent denied ──────────────────────────────────

test_byoa_unapproved_agent_denied if {
	not allow with input as {
		"step": {"agent_type": "evil-agent"},
		"agent": {"capability": "untrusted", "wasm_name": "evil-agent"},
	}
		with data.config.approved_external_agents as _approved
}

# ── Unknown agents denied ────────────────────────────────────────────────────

test_unknown_agent_denied if {
	not allow with input as {
		"step": {"agent_type": "shell"},
		"agent": {"capability": "exec", "wasm_name": "shell"},
	}
}

test_deny_message_for_unknown if {
	deny_message == "agent 'shell' (capability: 'exec') is not permitted for step 'shell'" with input as {
		"step": {"agent_type": "shell"},
		"agent": {"capability": "exec", "wasm_name": "shell"},
	}
}
