package wasm_af.submit

import rego.v1

default allow := false

# Only accept chat task types.
allow if {
	input.task_type == "chat"
}
