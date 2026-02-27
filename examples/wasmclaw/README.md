# Wasmclaw

A personal AI assistant with multi-skill routing, where every skill runs in a WASM sandbox gated by OPA policy. Intended as a more secure alternative to the typical "give the LLM a subprocess" agent pattern.

```bash
make demo
```

Prerequisites: Rust (wasm32-unknown-unknown + wasm32-wasip1 targets), Go 1.25+, jq, and either `nats-server` or `wash` (for its bundled NATS). Optional: `opa` CLI (for static policy tests), `wasm-tools` (for binary import analysis).

---

## What it demonstrates

### Two-tier execution model

LLM-generated code doesn't need to run on the host. Wasmclaw provides two execution tiers with different trust boundaries:

| | sandbox-exec | shell |
|---|---|---|
| **Where code runs** | Inside Wazero (WASM VM) | Host OS via `exec.Command` |
| **Trust boundary** | Wazero runtime (hypervisor-level) | OPA policy + Go host function |
| **Network** | None (WASI preview-1 has no sockets) | Host network |
| **Filesystem** | Only explicitly mounted paths | Confined by OPA path allowlist |
| **Policy posture** | Permissive (arbitrary code is safe) | Restrictive (binary + path + metachar gates) |

The router LLM prefers sandbox-exec for computation and code execution, reserving shell for host introspection (`ls`, `date`, `uname`).

### Defense-in-depth shell hardening

Shell commands are executed via `exec.Command(binary, args...)` — never `/bin/sh -c`. Shell metacharacters (`;`, `|`, `&`, `` ` ``, `$(`, `>`, `<`) are inert. Three independent enforcement layers:

1. **OPA policy** (primary gate): validates command binary against `allowed_commands`, rejects metacharacters, and confines all path arguments to `allowed_paths`. The policy is evaluated before any WASM or OS process runs.
2. **Host function** (defense-in-depth): validates binary and path arguments against its own static allowlists, independent of OPA. Restricts environment to `PATH=/usr/bin:/bin` (no host secret leakage).
3. **WASI-native agents** (preferred): file-ops uses WASI `std::fs` with Wazero-enforced `AllowedPaths`. No host function sits in the trust path. A prompt cannot add an import to a compiled binary.

### Policy-gated dynamic routing

The LLM router proposes a skill and parameters. Before the skill step is spliced into the plan, OPA validates the proposal against `allowed_skills`. Each skill step is then independently evaluated by OPA before execution. The policy is deny-by-default — agents not covered by a rule never run.

### Fail-closed design

- OPA policy is required. The orchestrator refuses to start without `OPA_POLICY`.
- Nil-policy paths return deny, not allow — both for step evaluation and task submission.
- Router splice fails closed: if policy is unavailable, the skill step is not inserted.

### Sandboxed code execution (Python-in-WASM)

The sandbox-exec agent runs LLM-generated Python inside a Wazero instance. The runtime binary comes from [vmware-labs/webassembly-language-runtimes](https://github.com/vmware-labs/webassembly-language-runtimes) (VMware, SHA256-verified). Compiled modules are cached; each invocation gets an isolated instance with its own memory, filesystem mounts, and stdio. No network access (WASI preview-1 has no sockets).

---

## Plan shape

```
POST /message { message: "calculate fibonacci of 10" }
  │
  ├── webhook-gateway → POST /tasks { type: "chat", query: "..." }
  │
  ├── submit.rego: allow (task_type == "chat")
  │
  ├── ChatBuilder → plan:
  │     memory(get) → router → [splice slot] → responder → memory(append)
  │
  ├── runStep(memory): kv_get conversation history
  ├── runStep(router): LLM classifies → {"skill":"sandbox-exec","params":{...}}
  │
  ├── spliceRoutedStep:
  │     policy.rego: router-splice allow (sandbox-exec ∈ allowed_skills)
  │     → insert sandbox-exec step into plan
  │
  ├── runStep(sandbox-exec):
  │     policy.rego: allow (sandbox_exec_enabled, python ∈ allowed_languages)
  │     → SandboxEngine.Exec: Wazero instance → python.wasm → stdout: "55"
  │
  ├── runStep(responder): LLM summarizes skill output
  └── runStep(memory): kv_put updated history
```

## Agents

| Agent | Capability | Host functions | Sandbox enforcement |
|---|---|---|---|
| memory | kv | kv_get, kv_put | NATS JetStream KV |
| router | llm | llm_complete | LLM output parsed as JSON |
| shell | shell | exec_command | `exec.Command` + OPA path/binary/metachar gates |
| file-ops | file | (none) | WASI std::fs, Wazero AllowedPaths |
| sandbox-exec | sandbox | sandbox_exec | Wazero instance per invocation, no network |
| web-search | http | (none) | Extism AllowedHosts |
| responder | llm | llm_complete | LLM only, no I/O |

## Files

- `agents.json` — agent registry: WASM binary names, capabilities, host functions, payload field mappings
- `data.json` — OPA external data: command allowlists, path allowlists, language allowlists, feature flags
- `policy.rego` — step policy: per-agent authorization rules, shell hardening helpers, config injection
- `submit.rego` — submission policy: restricts task types to `chat`
- `policy_test.rego` / `submit_test.rego` — 42 OPA unit tests proving security properties statically
- `Makefile` — `make demo` (build + run), `make test-policy` (OPA tests only), `make clean`
- `run.sh` — builds, starts services, runs OPA tests, then exercises every agent and security boundary
