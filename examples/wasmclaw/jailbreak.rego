package wasm_af.jailbreak

import rego.v1

# Standalone jailbreak scanner for ad-hoc use with `opa eval`.
# The real enforcement is the policy gate in policy.rego (email_reply_jailbreak),
# which checks prior_results at step evaluation time. This file is a convenience
# tool for scanning email JSON directly:
#
#   echo '{"emails":[...]}' | opa eval -I \
#       -d jailbreak.rego -d data.json 'data.wasm_af.jailbreak.safe'

default safe := true

safe := false if {
	some email in input.emails
	some pattern in data.config.jailbreak_patterns
	contains(lower(email.body), pattern)
}

violations contains msg if {
	some email in input.emails
	some pattern in data.config.jailbreak_patterns
	contains(lower(email.body), pattern)
	msg := sprintf("email from <%s> matched '%s'", [email.from, pattern])
}
