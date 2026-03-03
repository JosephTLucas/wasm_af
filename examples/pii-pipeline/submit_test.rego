package wasm_af.submit

import rego.v1

_allowed_types := ["pii-pipeline"]

# ── Functionality: permitted task type accepted ──────────────────────────────

test_pii_pipeline_allowed if {
	allow with input as {"task_type": "pii-pipeline"}
		with data.config.allowed_task_types as _allowed_types
}

# ── Security: unlisted task types denied ─────────────────────────────────────

test_chat_denied if {
	not allow with input as {"task_type": "chat"}
		with data.config.allowed_task_types as _allowed_types
}

test_arbitrary_type_denied if {
	not allow with input as {"task_type": "evil-task"}
		with data.config.allowed_task_types as _allowed_types
}

test_empty_type_denied if {
	not allow with input as {"task_type": ""}
		with data.config.allowed_task_types as _allowed_types
}
