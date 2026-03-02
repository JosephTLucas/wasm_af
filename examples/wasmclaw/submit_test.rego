package wasm_af.submit

_allowed_types := ["chat", "email-reply", "skill-demo"]

# ── Functionality: permitted task types are accepted ─────────────────────────

test_chat_allowed if {
	allow with input as {"task_type": "chat"}
		with data.config.allowed_task_types as _allowed_types
}

test_email_reply_allowed if {
	allow with input as {"task_type": "email-reply"}
		with data.config.allowed_task_types as _allowed_types
}

test_skill_demo_allowed if {
	allow with input as {"task_type": "skill-demo"}
		with data.config.allowed_task_types as _allowed_types
}

# ── Security: unlisted task types are denied ─────────────────────────────────

test_research_denied if {
	not allow with input as {"task_type": "research"}
		with data.config.allowed_task_types as _allowed_types
}

test_unlisted_type_denied if {
	not allow with input as {"task_type": "unlisted-task"}
		with data.config.allowed_task_types as _allowed_types
}

test_empty_type_denied if {
	not allow with input as {"task_type": ""}
		with data.config.allowed_task_types as _allowed_types
}

test_arbitrary_type_denied if {
	not allow with input as {"task_type": "evil-task"}
		with data.config.allowed_task_types as _allowed_types
}
