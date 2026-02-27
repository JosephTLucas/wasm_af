# WASM_AF — WebAssembly Agent Framework

**A security-first AI agent orchestration framework built on WebAssembly and [Extism](https://extism.org/).**

WASM_AF leverages the sandboxed, ephemeral nature of WebAssembly to create a zero-trust AI agent runtime. Agents are WASM plugins — isolated by default, granted capabilities by policy, and destroyed when their work is done. No ambient authority. No lateral movement. No leaked secrets.

---

## Quick Start

```bash
# Fan-out summarizer (builds everything, fetches 3 URLs in parallel, summarizes)
./examples/fan-out-summarizer/run.sh

# Wasmclaw personal assistant — with NVIDIA NIM API inference
LLM_MODE=api NV_API_KEY="nvapi-..." ./examples/wasmclaw/run.sh
```

Prerequisites: [Rust](https://rustup.rs/), [Go](https://go.dev/) 1.25+, [NATS server](https://nats.io/) (or [wash](https://wasmcloud.com/docs/installation/) which bundles one), [jq](https://jqlang.github.io/jq/).

For API inference, set `NV_API_KEY` in a `.env` file at the repo root (gitignored) or export it in your shell.

---

## Why WASM + AI Agents?

Most agent frameworks enforce security through **convention**: configure your tools carefully, don't pass credentials to agents that don't need them, restrict network access through application-level checks. These conventions work — until a prompt injection, a misconfiguration, or a supply-chain compromise bypasses them. The boundary between "allowed" and "not allowed" is maintained by the same process that the attacker controls.

WebAssembly enforces security through **construction**. A WASM module **cannot** touch the filesystem, network, or environment unless explicitly granted access by the host. This isn't a policy layer bolted on top — it's a property of the execution model itself. A prompt injection can manipulate what an agent *tries* to do, but it cannot expand what the sandbox *permits*.

WASM_AF is built on the premise that this is exactly the runtime model AI agents need: capabilities granted structurally at instantiation time, not checked at call time.

### What structural enforcement looks like in practice

**Capability boundaries are set at plugin creation, not at call time.** When the orchestrator creates a url-fetch plugin, it passes `allowed_hosts: ["webassembly.org"]` in the Extism manifest. The wazero runtime enforces this at the syscall layer. If the agent — whether through a bug, a prompt injection, or malicious code — tries to reach `evil.com`, the request never leaves the sandbox. There is no "check" to bypass; the capability doesn't exist.

**Missing imports are absolute.** The summarizer plugin's compiled WASM binary contains an import for `llm_complete` but no import for `http_request`. This is verifiable: `wasm-tools print summarizer.wasm | grep import`. A prompt injection cannot add an import to a compiled binary. The LLM can instruct the agent to make HTTP calls all day; the function doesn't exist in the module's address space.

**Credentials never enter the sandbox.** The LLM API key lives in a Go closure inside the orchestrator. It is passed directly from Go to the upstream HTTP client — never serialized into the `TaskInput` struct, never written to WASM linear memory. The agent cannot leak what it never had.

---

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                  Go Orchestrator Binary                      │
│                                                              │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────────┐  │
│  │ HTTP API │  │ Plan Builder │  │ Task State (NATS KV)  │  │
│  │ :8080    │──│              │──│ + Audit Log           │  │
│  └──────────┘  └──────┬───────┘  └───────────────────────┘  │
│                       │                                      │
│            ┌──────────┴──────────┐                           │
│            │     Step Runner     │                           │
│            │  (parallel dispatch)│                           │
│            └──┬──────┬──────┬───┘                           │
│               │      │      │    per step:                   │
│            ┌──▼──┐┌──▼──┐┌──▼──┐  create plugin             │
│            │fetch││fetch││fetch│  → inject scoped caps       │
│            │ .wasm││.wasm││.wasm│  → call execute()          │
│            └──┬──┘└──┬──┘└──┬──┘  → destroy plugin           │
│               │      │      │                                │
│  ┌────────────▼──────▼──────▼────────────┐                   │
│  │        Extism Runtime (wazero)        │                   │
│  │  • allowed_hosts per instance         │                   │
│  │  • host functions via registry        │                   │
│  │  • config, allowed_paths per step     │                   │
│  │  • memory limits, timeout per step    │                   │
│  │  • WASM sandbox (no ambient authority)│                   │
│  └───────────────────────────────────────┘                   │
│                                                              │
│  ┌──────────────────────────────────────────┐                │
│  │  OPA Policy Engine (embedded)           │                │
│  │  • data.wasm_af.authz — step policy     │                │
│  │  • data.wasm_af.submit — submit policy  │                │
│  │  • data store ← NATS KV live updates    │                │
│  │  • structured decisions: allowed_hosts, │                │
│  │    memory, timeout, config, paths,      │                │
│  │    host_functions                       │                │
│  └──────────────────────────────────────────┘                │
│                                                              │
│  ┌──────────────────────────────────────────┐                │
│  │  Host Function Registry                 │                │
│  │  • providers registered by name         │                │
│  │  • resolved dynamically per step        │                │
│  │  • policy can override/filter           │                │
│  └──────────────────────────────────────────┘                │
└──────────────────────────────────────────────────────────────┘
```

The orchestrator is a single Go binary that embeds the [Extism](https://extism.org/) WASM runtime (powered by [wazero](https://wazero.io/) — pure Go, no CGO). For each task step, it creates an Extism plugin instance with exactly the capabilities that step needs, calls the agent's `execute` export, reads the result, and destroys the plugin. The WASM instance, its memory, and its capabilities cease to exist.

NATS JetStream KV provides task state persistence and an immutable audit trail. It's the only external dependency.

---

## Core Principles

### 1. Policy-Driven Capability Grants

An agent does not declare what it needs — the **orchestrator decides what it gets**, constrained by policy.

When a task requires a url-fetch agent, the orchestrator:

1. Evaluates the OPA step policy (`wasm_af.authz`) with full task/step/agent context
2. Reads structured decisions from the policy — `allowed_hosts`, `max_memory_pages`, `timeout_sec`
3. Creates an Extism plugin with exactly the capabilities the policy granted
4. Calls the agent's `execute` export
5. Destroys the plugin — along with its capabilities and any in-memory data

The agent's HTTP access is enforced by the Extism runtime, not application code. The domain allowlist, the allowed_hosts scoping, and the resource limits are all expressed in Rego — one language, one decision point.

### 2. Per-Instance Capability Scoping

Each plugin instance gets its own Extism manifest. In the fan-out example, three url-fetch instances run in parallel — one scoped to `webassembly.org`, one to `wasmcloud.com`, one to `bytecodealliance.org`. Instance A literally cannot reach Instance B's domain. The runtime rejects the request before it leaves the sandbox.

The allowlist is data-driven: `data.config.allowed_domains` in the OPA data store, populated from `data.json` at startup. NATS KV watches push live updates into the OPA store — the orchestrator picks them up on the next policy evaluation, no restart needed:

```bash
# Update the domain allowlist live (pushed into OPA data store automatically)
nats kv put wasm-af-config allowed-fetch-domains "webassembly.org,wasmcloud.com,bytecodealliance.org"

# Clear all domain restrictions
nats kv del wasm-af-config allowed-fetch-domains
```

### 3. Zero-Trust Inter-Agent Communication

Agents do not talk to each other. All inter-agent communication is mediated by the orchestrator.

If a task decomposes into `fetch → fetch → fetch → summarize`, the orchestrator runs each step, stores intermediate results in NATS KV, and passes accumulated context to the next step. Each agent sees only its own inputs and outputs. No agent has a handle to another agent.

### 4. LLM Access as a Gated Capability

LLM inference is delivered as a **host function** that the orchestrator injects into plugins that need it. The summarizer plugin receives an `llm_complete` host function. The url-fetch plugin does not — the function doesn't exist in its WASM instance. Even if a prompt injection tries to call it, the import is missing from the module's address space.

The LLM backend is configurable: mock (deterministic, no network), NVIDIA NIM API (remote inference via OpenAI-compatible endpoint), or local Ollama. In all modes, credentials live in a Go closure — never serialized into WASM memory.

### 5. Lifecycle as a Security Primitive

WASM plugins are designed for rapid instantiation. WASM_AF treats this as a security feature, not just a performance one.

An agent that doesn't exist can't be exploited. Between steps, there is no running process to attack, no memory to dump, no socket to probe. Each plugin is created, used, and destroyed within a single Go function scope — `NewPlugin` → `Call` → `Close`. This isn't a convention — it's the code path.

### 6. Resource Limits

Each plugin instance is created with configurable resource constraints:

- **Execution timeout** (`PLUGIN_TIMEOUT_SEC`, default 30s): cancels the WASM execution if it exceeds the deadline. Prevents infinite loops or stalling from prompt-injected logic.
- **Memory limit** (`PLUGIN_MAX_MEMORY_PAGES`, default 256 pages = 16 MiB): caps the linear memory a plugin can allocate. One WASM page is 64 KiB.
- **HTTP response size** (`PLUGIN_MAX_HTTP_BYTES`, default 4 MiB): limits the size of HTTP responses a plugin can read.

These limits are enforced by the wazero runtime, not by the plugin code.

---

## Project Structure

```
wasm_af/
├── go.mod                          # Go module (orchestrator)
├── Makefile                        # build, test, demo
│
├── provider/orchestrator/          # the framework — Go binary
│   ├── main.go                     # standalone binary, env config, HTTP server
│   ├── orchestrator.go             # Extism plugin lifecycle, param enrichment
│   ├── policy.go                   # OPA evaluator (compiles Rego, evaluates per step)
│   ├── loop.go                     # plan execution, parallel dispatch, router splice
│   ├── hostfns.go                  # host function registry (dynamic, name-based)
│   ├── hostfns_shell.go            # exec_command: exec.Command + path/binary/metachar gates
│   ├── hostfns_memory.go           # kv_get/kv_put: NATS JetStream KV
│   ├── hostfns_sandbox.go          # sandbox_exec: runs code in a nested Wazero instance
│   ├── hostfns_email.go            # send_email: SMTP delivery via host fn (creds in closure)
│   ├── llm.go                      # llm_complete host function provider (mock / API / Ollama)
│   ├── registry.go                 # agent registry with enrichments
│   ├── builders.go                 # plan builders (including generic JSON-driven)
│   ├── builder_chat.go             # chat plan builder (memory → router → splice → responder)
│   └── taskstate.go                # HTTP handlers
│
├── cmd/webhook-gateway/            # lightweight HTTP gateway (chat message → task → poll)
│
├── pkg/taskstate/                  # NATS JetStream KV: task state, audit log, payloads
│
├── runtimes/                       # WASI sandbox runtimes (downloaded, not checked in)
│   ├── build.sh                    # downloads Python WASM from VMware Labs (SHA256-verified)
│   └── python.wasm                 # CPython 3.12 for wasm32-wasi (gitignored)
│
├── components/                     # Rust workspace — WASM plugins (Extism PDK)
│   ├── agent-types/                # shared TaskInput/TaskOutput types
│   ├── agents/
│   │   ├── router/                 # LLM-based skill router (classifies → skill + params)
│   │   ├── shell/                  # host command execution via exec_command host fn
│   │   ├── sandbox-exec/           # sandboxed code execution via sandbox_exec host fn
│   │   ├── file-ops/               # WASI std::fs (wasm32-wasip1, no host functions)
│   │   ├── email-send/              # host fn email delivery (SMTP creds never in WASM)
│   │   ├── email-read/             # sandboxed inbox reader (OPA-injected API key)
│   │   ├── memory/                 # conversation history via kv_get/kv_put
│   │   ├── responder/              # LLM response generation
│   │   ├── url-fetch/              # fetches a URL, returns page content
│   │   ├── web-search/             # calls Brave Search API
│   │   └── summarizer/             # builds LLM prompt from search results
│
└── examples/
    ├── fan-out-summarizer/         # parallel fetch + summarize demo
    │   ├── run.sh, policy.rego, data.json, README.md, ...
    │
    ├── prompt-injection/           # security demo: injection fails structurally
    │   ├── run.sh, malicious_page.html, policy.rego, Makefile, README.md, ...
    │
    └── wasmclaw/                   # personal AI assistant with two-tier execution
        ├── run.sh                  # builds, runs, exercises every agent + security boundary
        ├── agents.json             # agent registry (9 agents, capability/host-fn mappings)
        ├── policy.rego             # step policy: authz, shell hardening, email-reply jailbreak gate
        ├── jailbreak.rego          # standalone jailbreak scanner (ad-hoc opa eval)
        ├── data.json               # allowlists, feature flags, jailbreak patterns
        ├── policy_test.rego        # OPA authz tests (metachar, path, splice, jailbreak gate)
        ├── jailbreak_test.rego     # OPA jailbreak scanner tests
        ├── Makefile                # make demo, make demo-api, make test-policy
        └── README.md
```

**Go for the orchestrator.** It's a coordination problem — HTTP API, NATS, goroutine fan-out, plugin lifecycle management. The Extism Go SDK (wazero) is pure Go with no CGO, so the binary cross-compiles cleanly.

**Rust for plugins.** Agents compile to WASM via `wasm32-unknown-unknown`. Rust produces tiny binaries (~150KB), and the Extism Rust PDK provides HTTP, config, and host function access with minimal boilerplate. Shared types (`TaskInput`, `TaskOutput`, `KVPair`) live in the `agent-types` crate to avoid duplication.

**OPA for policy.** The [Open Policy Agent](https://www.openpolicyagent.org/) Go library evaluates Rego policies natively in the orchestrator. Two decision points: `wasm_af.authz` (step execution — deny-by-default) and `wasm_af.submit` (task submission). Policies are data-driven: the domain allowlist lives in OPA's data store, updated live from NATS KV without restart. Structured decisions let policy shape *how* plugins run (resource limits, network scoping), not just *whether* they run. Policies are testable with `opa test`.

---

## Running the Demos

### Wasmclaw (Personal AI Assistant)

Multi-skill chat assistant with two-tier execution: LLM-generated code runs in a Python-in-WASM sandbox (Wazero), while host commands use `exec.Command` with OPA binary/path/metachar gates. The demo exercises every agent and proves each security boundary at runtime.

```bash
# Mock LLM (deterministic routing, no external dependencies)
./examples/wasmclaw/run.sh

# NVIDIA NIM API (remote inference — needs NV_API_KEY in .env or environment)
LLM_MODE=api ./examples/wasmclaw/run.sh

# Local Ollama
LLM_MODE=real ./examples/wasmclaw/run.sh
```

### Fan-Out Summarizer

Fetches 3 URLs in parallel (each in its own sandbox with per-instance network scoping), merges the results, and produces a summary. Demonstrates capability scoping, policy gating, cross-instance isolation, and live allowlist updates.

```bash
./examples/fan-out-summarizer/run.sh
```

### Prompt Injection

A url-fetch agent fetches a page containing a hidden prompt injection that instructs the LLM to exfiltrate credentials. The model may follow the instruction — but nothing is exfiltrated, because the sandbox structurally prevents it. The demo uses `wasm-tools` to inspect the compiled binary's imports as proof.

```bash
cd examples/prompt-injection && make demo
```

Requires [Ollama](https://ollama.com) (the Makefile pulls `gemma3:270m` automatically).

### Manual Step-by-Step

```bash
# 1. Build
cd components && cargo build --release && cd ..
go build -o ./bin/orchestrator ./provider/orchestrator/

# 2. Start NATS
nats-server -js &

# 3. Run orchestrator
OPA_POLICY=./examples/fan-out-summarizer \
OPA_DATA=./examples/fan-out-summarizer/data.json \
AGENT_REGISTRY_FILE=./examples/fan-out-summarizer/agents.json \
LLM_MODE=mock \
./bin/orchestrator &

# 4. Submit task
curl -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "fan-out-summarizer",
    "query": "Compare these WebAssembly projects",
    "context": {"urls": "https://webassembly.org,https://wasmcloud.com,https://bytecodealliance.org"}
  }'

# 5. Poll result
curl localhost:8080/tasks/<task-id> | jq .
```

---

## Configuration

| Variable | Default | Description |
|---|---|---|
| `WASM_DIR` | `./components/target/wasm32-unknown-unknown/release` | Directory containing compiled `.wasm` plugins |
| `NATS_URL` | `nats://127.0.0.1:4222` | NATS server address |
| `OPA_POLICY` | — | Path to `.rego` file or directory (**required**) |
| `OPA_DATA` | — | Path to a JSON data file (populates OPA data store at startup) |
| `AGENT_REGISTRY_FILE` | — | Path to JSON agent registry file (required) |
| `AGENT_REGISTRY` | — | Inline JSON agent registry (takes precedence over file) |
| `LLM_MODE` | `mock` | `mock` for deterministic routing, `api` for remote inference (NVIDIA NIM, etc.), `real` for local Ollama |
| `LLM_BASE_URL` | — | OpenAI-compatible API base URL (auto-detected: `/v1` suffix handled correctly) |
| `LLM_API_KEY` | — | API key for the LLM endpoint (required when `LLM_MODE=api`) |
| `LLM_MODEL` | `gpt-4o-mini` | Model name for the LLM endpoint |
| `LLM_TEMPERATURE` | — | Default sampling temperature (applied when the agent doesn't specify one) |
| `LLM_TOP_P` | — | Default nucleus sampling parameter (applied when the agent doesn't specify one) |
| `PLUGIN_TIMEOUT_SEC` | `30` | Max wall-clock seconds per plugin invocation |
| `PLUGIN_MAX_MEMORY_PAGES` | `256` | Max WASM memory pages per plugin (64 KiB each) |
| `PLUGIN_MAX_HTTP_BYTES` | `4194304` | Max HTTP response size in bytes per plugin |
| `SHELL_ALLOWED_COMMANDS` | `ls,cat,pwd,...` | Comma-separated command binary allowlist (host-side defense-in-depth) |
| `SHELL_ALLOWED_PATHS` | `/tmp/wasmclaw` | Comma-separated path bases for shell argument confinement |
| `SANDBOX_RUNTIMES_DIR` | `./runtimes` | Directory containing WASI runtime `.wasm` files (e.g. `python.wasm`) |
| `SANDBOX_TIMEOUT_SEC` | `30` | Max wall-clock seconds per sandboxed code execution |
| `SANDBOX_ALLOWED_LANGUAGES` | `python` | Comma-separated language allowlist for sandbox-exec |
| `SANDBOX_ALLOWED_PATHS` | `/tmp/wasmclaw` | Comma-separated host paths mounted into sandbox instances |

---

## Open Questions

- **Agent-to-agent streaming.** For long-running LLM generations, should agents stream tokens through the orchestrator, or should direct plugin-to-plugin channels be permitted under policy?
- **Recursive decomposition.** When an agent determines it needs another agent, the request must round-trip through the orchestrator. Is the latency cost acceptable?
- **Token budgeting.** The LLM host function is the natural enforcement point for per-task token limits. How should budgets propagate when a task fans out?
- **Distribution.** The current architecture is single-process. For multi-host scaling, multiple orchestrator instances could distribute work over NATS. The mechanism exists but isn't built yet.
- **Component Model.** Extism currently uses core WASM modules. When the Go ecosystem gains Component Model hosting support, migrating to WIT-typed interfaces would add compile-time safety at the plugin boundary.

---

## License

TBD
