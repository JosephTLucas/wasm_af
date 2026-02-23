# WASM_AF — WebAssembly Agent Framework

**A security-first AI agent orchestration framework built on WebAssembly and [Extism](https://extism.org/).**

WASM_AF leverages the sandboxed, ephemeral nature of WebAssembly to create a zero-trust AI agent runtime. Agents are WASM plugins — isolated by default, granted capabilities by policy, and destroyed when their work is done. No ambient authority. No lateral movement. No leaked secrets.

---

## Quick Start

```bash
# Run the fan-out summarizer demo (builds everything, fetches 3 URLs in parallel, summarizes)
./examples/fan-out-summarizer/run.sh
```

Prerequisites: [Rust](https://rustup.rs/), [Go](https://go.dev/) 1.22+, [NATS server](https://nats.io/) (or [wash](https://wasmcloud.com/docs/installation/) which bundles one), [jq](https://jqlang.github.io/jq/).

---

## Why WASM + AI Agents?

Today's agent frameworks treat security as an afterthought. Agents run in shared processes with broad network access, ambient credentials, and the ability to spawn arbitrary subprocesses. A prompt injection in one tool call can compromise the entire system.

WebAssembly flips this model. A WASM module **cannot** touch the filesystem, network, or environment unless explicitly granted access by the host. This isn't a policy layer bolted on top — it's a property of the execution model itself. WASM_AF is built on the premise that this is exactly the runtime model AI agents need.

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

### 3. Zero-Trust Inter-Agent Communication

Agents do not talk to each other. All inter-agent communication is mediated by the orchestrator.

If a task decomposes into `fetch → fetch → fetch → summarize`, the orchestrator runs each step, stores intermediate results in NATS KV, and passes accumulated context to the next step. Each agent sees only its own inputs and outputs. No agent has a handle to another agent.

### 4. LLM Access as a Gated Capability

LLM inference is delivered as a **host function** that the orchestrator injects into plugins that need it. The summarizer plugin receives an `llm_complete` host function. The url-fetch plugin does not — the function doesn't exist in its WASM instance. Even if a prompt injection tries to call it, the import is missing from the module's address space.

### 5. Lifecycle as a Security Primitive

WASM plugins start in sub-milliseconds. WASM_AF treats this as a security feature, not just a performance one.

An agent that doesn't exist can't be exploited. Between steps, there is no running process to attack, no memory to dump, no socket to probe. Each plugin is created, used, and destroyed within a single Go function scope — `NewPlugin` → `Call` → `Close`. This isn't a convention — it's the code path.

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
│   ├── agents/
│   │   ├── url-fetch/              # fetches a URL, returns page content
│   │   ├── web-search/             # calls Brave Search API
│   │   └── summarizer/             # builds LLM prompt, calls llm_complete host fn
│   └── policy-engine/              # deny-by-default policy evaluation
│
└── examples/
    └── fan-out-summarizer/         # end-to-end demo
        ├── run.sh                  # one command: build + run + display results
        ├── policies.json           # policy rules for the demo
        └── README.md
```

**Go for the orchestrator.** It's a coordination problem — HTTP API, NATS, goroutine fan-out, plugin lifecycle management. The Extism Go SDK (wazero) is pure Go with no CGO, so the binary cross-compiles cleanly.

**Rust for plugins.** Agents and the policy engine compile to WASM via `wasm32-unknown-unknown`. Rust produces tiny binaries (~150KB), and the Extism Rust PDK provides HTTP, config, and host function access with minimal boilerplate.

---

## Running the Demo

The fan-out summarizer fetches 3 URLs in parallel (each in its own sandbox with per-instance network scoping), merges the results, and produces a summary.

```bash
./examples/fan-out-summarizer/run.sh
```

Or step by step:

```bash
# 1. Build
cd components && cargo build --release && cd ..
go build -o ./bin/orchestrator ./provider/orchestrator/

# 2. Start NATS
nats-server -js &

# 3. Run orchestrator
POLICY_RULES_FILE=./examples/fan-out-summarizer/policies.json \
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

## Relationship to Existing Agent Frameworks

Frameworks like LangChain, CrewAI, and similar libraries operate at a different layer. They are Python libraries for wiring together agents that run as functions in shared processes with full ambient system access. Their value is in connecting and optimizing agents. WASM_AF's value is in ensuring agents *cannot access anything they haven't been explicitly granted*.

These are different execution models, not different flavors of the same idea.

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
