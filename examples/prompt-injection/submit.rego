package wasm_af.submit

import rego.v1

default allow := false

allow if {
	input.task_type in data.config.allowed_task_types
}

deny_message := msg if {
	not allow
	msg := sprintf("task type %q is not allowed", [input.task_type])
}
