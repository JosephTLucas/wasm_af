package wasm_af.submit

# ── Functionality: chat tasks are accepted ────────────────────────────────────

test_chat_allowed if {
	allow with input as {"task_type": "chat"}
}

# ── Security: all other task types are denied ─────────────────────────────────

test_research_denied if {
	not allow with input as {"task_type": "research"}
}

test_fan_out_denied if {
	not allow with input as {"task_type": "fan-out-summarizer"}
}

test_empty_type_denied if {
	not allow with input as {"task_type": ""}
}

test_arbitrary_type_denied if {
	not allow with input as {"task_type": "evil-task"}
}

# ── Functionality: email-reply tasks are accepted ────────────────────────────

test_email_reply_allowed if {
	allow with input as {"task_type": "email-reply"}
}
