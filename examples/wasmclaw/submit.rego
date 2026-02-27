package wasm_af.submit

import rego.v1

default allow := false

allow if {
	input.task_type == "chat"
}

allow if {
	input.task_type == "email-reply"
}
