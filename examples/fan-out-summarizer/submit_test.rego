package wasm_af.submit

import rego.v1

test_allowed_task_type if {
	allow with input as {"task_type": "fan-out-summarizer"}
		with data.config.allowed_task_types as ["fan-out-summarizer", "research"]
}

test_denied_task_type if {
	not allow with input as {"task_type": "dangerous-task"}
		with data.config.allowed_task_types as ["fan-out-summarizer", "research"]
}
