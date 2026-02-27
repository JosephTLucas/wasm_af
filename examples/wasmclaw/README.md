# Wasmclaw

Multi-skill chat assistant where every skill runs in a WASM sandbox gated by OPA policy.

```bash
make demo                              # mock LLM (deterministic routing)
LLM_MODE=api make demo                 # NVIDIA NIM API (needs NV_API_KEY)
LLM_MODE=real make demo                # local Ollama
```

For API inference, set `NV_API_KEY` in `.env` at the repo root. Default model: `nvdev/nvidia/llama-3.3-nemotron-super-49b-v1` (override with `NV_MODEL`).

Prerequisites: Rust (wasm32-unknown-unknown + wasm32-wasip1), Go 1.25+, jq, `nats-server`. Optional: `opa`, `wasm-tools`.

---

## Two-tier execution

| | sandbox-exec | shell |
|---|---|---|
| **Runs in** | Wazero (WASM VM) | Host OS via `exec.Command` |
| **Trust boundary** | Wazero runtime | OPA + Go host function |
| **Network** | None | Host network |
| **Filesystem** | Explicitly mounted paths only | OPA path allowlist |
| **Policy posture** | Permissive (code can't escape WASM) | Restrictive (binary + path + metachar gates) |

Shell commands use `exec.Command(binary, args...)` — never `/bin/sh -c`. Three enforcement layers: OPA policy (binary allowlist, metachar rejection, path confinement), host function (independent allowlist, `PATH=/usr/bin:/bin`), and WASI-native agents (file-ops uses `std::fs` with Wazero `AllowedPaths` — no host function in the trust path).

## Email agents

| | email-send | email-read |
|---|---|---|
| **Secret location** | Go closure (SMTP creds) | OPA-injected plugin config |
| **Host functions** | `send_email` | (none) |
| **Network** | None (host fn handles it) | None |
| **Injection defense** | Never sees credentials | No host fns, no HTTP, no exfil path |

## Email reply pipeline

```
email-read → [OPA jailbreak gate] → responder → email-send
```

The jailbreak check is the OPA policy evaluation before the responder step — it inspects `prior_results` for injection patterns configured in `data.json`. Two demo scenarios:

| Scenario | Target email | Result |
|---|---|---|
| **A** | Clean (alice, Q3 Planning) | `✓ read → ✓ respond → ✓ send` |
| **B** | Prompt injection attempt | `✓ read → ✗ responder denied` — LLM never sees injected content |

---

## Plan shape

```
POST /message { message: "calculate fibonacci of 10" }
  │
  ├── webhook-gateway → POST /tasks { type: "chat", query: "..." }
  ├── submit.rego: allow (task_type == "chat")
  │
  ├── ChatBuilder → plan:
  │     memory(get) → router → [splice slot] → responder → memory(append)
  │
  ├── runStep(memory): kv_get conversation history
  ├── runStep(router): LLM → {"skill":"sandbox-exec","params":{...}}
  │
  ├── spliceRoutedStep:
  │     policy.rego: router-splice allow (sandbox-exec ∈ allowed_skills)
  │     → insert sandbox-exec step into plan
  │
  ├── runStep(sandbox-exec):
  │     policy.rego: allow (sandbox_exec_enabled, python ∈ allowed_languages)
  │     → Wazero instance → python.wasm → stdout: "55"
  │
  ├── runStep(responder): LLM summarizes skill output
  └── runStep(memory): kv_put updated history
```

## Agents

| Agent | Capability | Host functions | Enforcement |
|---|---|---|---|
| memory | kv | kv_get, kv_put | NATS JetStream KV |
| router | llm | llm_complete | JSON parse |
| shell | shell | exec_command | `exec.Command` + OPA gates |
| file-ops | file | (none) | WASI std::fs, Wazero AllowedPaths |
| sandbox-exec | sandbox | sandbox_exec | Wazero instance, no network |
| web-search | http | (none) | Extism AllowedHosts |
| email-send | email | send_email | Host fn; creds in Go closure |
| email-read | email | (none) | OPA-injected key; no network |
| responder | llm | llm_complete | LLM only, no I/O |

## Files

- `agents.json` — agent registry
- `data.json` — OPA data: allowlists, feature flags, jailbreak patterns
- `policy.rego` — step policy (authz, shell hardening, jailbreak gate)
- `jailbreak.rego` — standalone scanner for ad-hoc `opa eval`
- `submit.rego` — submission policy
- `*_test.rego` — 69 OPA tests (`opa test .`)
- `Makefile` / `run.sh` — build + run
