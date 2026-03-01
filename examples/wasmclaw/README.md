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

## Human-in-the-loop approval gates

High-stakes steps can require human approval before execution. Policy controls which agent types need approval:

- **email-send** always requires approval when `approval_enabled` is true
- **shell** commands not in `auto_approved_commands` require approval (e.g. `find` needs approval, `ls` does not)

When a step requires approval, the orchestrator pauses it in `awaiting_approval` status and publishes an event to NATS. The task continues executing other branches. Approve or reject via the API:

```bash
# List pending approvals
curl localhost:8080/tasks/<task-id>/approvals | jq .

# Approve a step
curl -X POST localhost:8080/tasks/<task-id>/steps/<step-id>/approve \
  -d '{"approved_by": "alice"}'

# Reject a step
curl -X POST localhost:8080/tasks/<task-id>/steps/<step-id>/reject \
  -d '{"rejected_by": "bob", "reason": "not appropriate"}'
```

Controlled by `data.json` config:
- `approval_enabled` — master switch for approval gates
- `auto_approved_commands` — shell commands that skip approval (safe read-only commands)

## Email reply pipeline

```
email-read → [OPA jailbreak gate] → responder → [approval gate] → email-send
```

The jailbreak check is the OPA policy evaluation before the responder step — it inspects `prior_results` for injection patterns configured in `data.json`.

## Reply-all parallel DAG

The `reply-all` task type processes all emails in a single DAG with parallel branches:

```
               email-read
              /          \
     responder-0       responder-1
     (email 0)         (email 1)
         |                 |
   email-send-0      email-send-1
   (approval)        (never runs)
```

Email 0 (clean) proceeds to email-send which pauses for human approval. Email 1 (prompt injection) is denied by the jailbreak gate — its branch dies without affecting the healthy branch.

```bash
make reply-all-demo           # standalone demo with interactive Y/n approval
```

---

## Plan shape

```
POST /message { message: "calculate fibonacci of 10" }
  │
  ├── webhook-gateway → POST /tasks { type: "chat", query: "..." }
  ├── submit.rego: allow (task_type == "chat")
  │
  ├── ChatBuilder → plan (DAG):
  │     memory(get) → router → responder → memory(append)
  │
  ├── runStep(memory): kv_get conversation history
  ├── runStep(router): LLM → {"skill":"sandbox-exec","params":{...}}
  │
  ├── handleSplice (router has "splice": true):
  │     policy.rego: router-splice allow (sandbox-exec ∈ allowed_skills)
  │     → insert sandbox-exec, rewire responder to depend on it
  │
  ├── runStep(sandbox-exec):
  │     policy.rego: allow (sandbox_exec_enabled, python ∈ allowed_languages)
  │     → Wazero instance → python.wasm → stdout: "55"
  │
  ├── runStep(responder): LLM summarizes skill output (ancestors: memory, router, sandbox-exec)
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

## Bring Your Own Agent (BYOA)

External WASM agents can be uploaded at runtime via `POST /agents`. They receive `capability: "untrusted"` and are sandboxed by the BYOA policy tier. To enable it, copy `policies/byoa.rego` alongside `policy.rego` — the rules merge automatically.

Approve external agents for execution by adding them to `data.json`:

```json
"approved_external_agents": ["my-custom-agent"]
```

Or update at runtime: `nats kv put wasm-af-config approved-external-agents "my-custom-agent"`.

See [docs/creating-an-agent.md](../../docs/creating-an-agent.md) section 8 for the full BYOA guide.

## Files

- `agents.json` — agent registry
- `data.json` — OPA data: allowlists, feature flags, jailbreak patterns, BYOA approved list
- `policy.rego` — step policy (authz, shell hardening, jailbreak gate, approval gates)
- `submit.rego` — submission policy (data-driven via `allowed_task_types`)
- `jailbreak.rego` — standalone scanner for ad-hoc `opa eval`
- `*_test.rego` — 80 OPA tests (`opa test .`)
- `lib/setup.sh` — shared infrastructure (build, NATS, orchestrator, cleanup)
- `run.sh` — main demo (skills, security, approval)
- `reply-all-demo.sh` — parallel DAG demo (jailbreak + approval in one task)
- `Makefile` — `make demo`, `make demo-api`, `make reply-all-demo`, `make test-policy`
