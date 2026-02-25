# WASM_AF — WebAssembly Agent Framework

**A security-first AI agent orchestration framework built on WebAssembly and [Extism](https://extism.org/).**

WASM_AF leverages the sandboxed, ephemeral nature of WebAssembly to create a zero-trust AI agent runtime. Agents are WASM plugins — isolated by default, granted capabilities by policy, and destroyed when their work is done. No ambient authority. No lateral movement. No leaked secrets.

---

## Quick Start

```bash
# Run the fan-out summarizer demo (builds everything, fetches 3 URLs in parallel, summarizes)
./examples/fan-out-summarizer/run.sh
```

Prerequisites: [Rust](https://rustup.rs/), [Go](https://go.dev/) 1.24+, [NATS server](https://nats.io/) (or [wash](https://wasmcloud.com/docs/installation/) which bundles one), [jq](https://jqlang.github.io/jq/).

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
│  │  • host functions (LLM) per instance  │                   │
│  │  • memory limits per instance         │                   │
│  │  • execution timeout per invocation   │                   │
│  │  • WASM sandbox (no ambient authority)│                   │
│  └───────────────────────────────────────┘                   │
│                                                              │
│  ┌──────────────────┐                                        │
│  │  Policy Engine   │  WASM plugin, deny-by-default          │
│  │  (policy.wasm)   │  evaluated before every step           │
│  └──────────────────┘                                        │
└──────────────────────────────────────────────────────────────┘
```

The orchestrator is a single Go binary that embeds the [Extism](https://extism.org/) WASM runtime (powered by [wazero](https://wazero.io/) — pure Go, no CGO). For each task step, it creates an Extism plugin instance with exactly the capabilities that step needs, calls the agent's `execute` export, reads the result, and destroys the plugin. The WASM instance, its memory, and its capabilities cease to exist.

NATS JetStream KV provides task state persistence and an immutable audit trail. It's the only external dependency.

---

## Core Principles

### 1. Policy-Driven Capability Grants

An agent does not declare what it needs — the **orchestrator decides what it gets**, constrained by policy.

When a task requires a url-fetch agent, the orchestrator:

1. Evaluates the policy engine (itself a WASM plugin — immutable, auditable)
2. Creates an Extism plugin with `allowed_hosts` scoped to the target domain
3. Calls the agent's `execute` export
4. Destroys the plugin — along with its capabilities and any in-memory data

The agent's HTTP access is enforced by the Extism runtime, not application code. The allow-list isn't advisory — it's the only network the agent can reach.

### 2. Per-Instance Capability Scoping

Each plugin instance gets its own Extism manifest. In the fan-out example, three url-fetch instances run in parallel — one scoped to `webassembly.org`, one to `wasmcloud.com`, one to `bytecodealliance.org`. Instance A literally cannot reach Instance B's domain. The runtime rejects the request before it leaves the sandbox.

The allowlist contents are controlled server-side via NATS KV — never populated from the task request itself. A submitted URL whose domain is absent is denied before any plugin is instantiated. The allowlist is stored in NATS KV and watched live by the orchestrator — it can be updated without restarting:

```bash
# Restrict to specific domains
nats kv put wasm-af-config allowed-fetch-domains "webassembly.org,wasmcloud.com,bytecodealliance.org"

# Open/dev mode — no domain restrictions
nats kv del wasm-af-config allowed-fetch-domains
```

`URL_FETCH_ALLOWED_DOMAINS` seeds the KV entry on first run if the key is absent. After that, the KV value is authoritative.

### 3. Zero-Trust Inter-Agent Communication

Agents do not talk to each other. All inter-agent communication is mediated by the orchestrator.

If a task decomposes into `fetch → fetch → fetch → summarize`, the orchestrator runs each step, stores intermediate results in NATS KV, and passes accumulated context to the next step. Each agent sees only its own inputs and outputs. No agent has a handle to another agent.

### 4. LLM Access as a Gated Capability

LLM inference is delivered as a **host function** that the orchestrator injects into plugins that need it. The summarizer plugin receives an `llm_complete` host function. The url-fetch plugin does not — the function doesn't exist in its WASM instance. Even if a prompt injection tries to call it, the import is missing from the module's address space.

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
│   ├── orchestrator.go             # Extism plugin lifecycle (create → call → destroy)
│   ├── loop.go                     # plan execution, parallel dispatch, context merging
│   ├── taskstate.go                # HTTP handlers, plan building
│   └── llm.go                      # LLM host function (mock + real OpenAI)
│
├── pkg/taskstate/                  # NATS JetStream KV: task state, audit log, payloads
│
├── components/                     # Rust workspace — WASM plugins (Extism PDK)
│   ├── agent-types/                # shared TaskInput/TaskOutput types
│   ├── agents/
│   │   ├── url-fetch/              # fetches a URL, returns page content
│   │   ├── web-search/             # calls Brave Search API
│   │   └── summarizer/             # builds LLM prompt, calls llm_complete host fn
│   └── policy-engine/              # deny-by-default policy evaluation
│
└── examples/
    ├── fan-out-summarizer/         # parallel fetch + summarize demo
    │   ├── run.sh                  # one command: build + run + display results
    │   ├── policies.json           # policy rules for the demo
    │   └── README.md
    └── prompt-injection/           # security demo: injection fails structurally
        ├── run.sh                  # builds, runs Ollama, demonstrates the attack
        ├── malicious_page.html     # page with hidden injection payload
        ├── policies.json           # policy rules
        ├── Makefile                # pulls model + builds
        └── README.md
```

**Go for the orchestrator.** It's a coordination problem — HTTP API, NATS, goroutine fan-out, plugin lifecycle management. The Extism Go SDK (wazero) is pure Go with no CGO, so the binary cross-compiles cleanly.

**Rust for plugins.** Agents and the policy engine compile to WASM via `wasm32-unknown-unknown`. Rust produces tiny binaries (~150KB), and the Extism Rust PDK provides HTTP, config, and host function access with minimal boilerplate. Shared types (`TaskInput`, `TaskOutput`, `KVPair`) live in the `agent-types` crate to avoid duplication.

---

## Running the Demos

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

Requires [Ollama](https://ollama.com) (the Makefile pulls `qwen3:1.7b` automatically).

### Manual Step-by-Step

```bash
# 1. Build
cd components && cargo build --release && cd ..
go build -o ./bin/orchestrator ./provider/orchestrator/

# 2. Start NATS
nats-server -js &

# 3. Run orchestrator
# URL_FETCH_ALLOWED_DOMAINS seeds NATS KV on first run; after that, update live with:
#   nats kv put wasm-af-config allowed-fetch-domains "domain1,domain2"
POLICY_RULES_FILE=./examples/fan-out-summarizer/policies.json \
AGENT_REGISTRY_FILE=./examples/fan-out-summarizer/agents.json \
LLM_MODE=mock \
URL_FETCH_ALLOWED_DOMAINS=webassembly.org,wasmcloud.com,bytecodealliance.org \
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
| `POLICY_RULES_FILE` | — | Path to JSON policy rules file |
| `POLICY_RULES` | — | Inline JSON policy rules (takes precedence over file) |
| `AGENT_REGISTRY_FILE` | — | Path to JSON agent registry file (required) |
| `AGENT_REGISTRY` | — | Inline JSON agent registry (takes precedence over file) |
| `LLM_MODE` | `mock` | `mock` for echo responses, `real` for upstream LLM |
| `LLM_BASE_URL` | — | OpenAI-compatible API base URL |
| `LLM_API_KEY` | — | API key for the LLM endpoint |
| `LLM_MODEL` | `gpt-4o-mini` | Model name for the LLM endpoint |
| `URL_FETCH_ALLOWED_DOMAINS` | — | Comma-separated domain allowlist (seeds NATS KV on first run) |
| `PLUGIN_TIMEOUT_SEC` | `30` | Max wall-clock seconds per plugin invocation |
| `PLUGIN_MAX_MEMORY_PAGES` | `256` | Max WASM memory pages per plugin (64 KiB each) |
| `PLUGIN_MAX_HTTP_BYTES` | `4194304` | Max HTTP response size in bytes per plugin |

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
