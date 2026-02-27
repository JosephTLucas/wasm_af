package wasm_af.submit

import rego.v1

default allow := false

allow if {
	input.task_type == "chat"
}

allow if {
	input.task_type == "email-reply"
}

deny_message := msg if {
	not allow
	msg := sprintf("task type %q is not allowed; permitted types: chat, email-reply", [input.task_type])
}
