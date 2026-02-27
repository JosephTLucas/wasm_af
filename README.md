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

WASM_AF is built on this premise: capabilities granted structurally at instantiation time, not checked at call time.

---

## Architecture

```
                 ┌──────────────────────────┐
                 │    Webhook Gateway       │   cmd/webhook-gateway
                 │    POST /message :8081   │   (chat entry point)
                 └────────────┬─────────────┘
                              │  POST /tasks
                              ▼
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

**Policy-driven capability grants.** An agent does not declare what it needs — the orchestrator decides what it gets. OPA evaluates every step before a plugin is created. Structured decisions from Rego (`allowed_hosts`, `max_memory_pages`, `timeout_sec`, `host_functions`, `config`, `allowed_paths`) flow directly into the Extism manifest. Deny-by-default; the orchestrator won't start without `OPA_POLICY`.

**Per-instance scoping.** Each plugin gets its own manifest. In the fan-out example, three url-fetch instances run in parallel — each scoped to exactly one domain. Instance A cannot reach Instance B's domain. The allowlist is data-driven and live-updatable via NATS KV:

```bash
nats kv put wasm-af-config allowed-fetch-domains "webassembly.org,wasmcloud.com"
```

**No inter-agent communication.** Agents do not talk to each other. The orchestrator mediates all data flow, stores intermediate results in NATS KV, and passes context from ancestor steps to their dependents.

**Host functions as capabilities.** LLM inference, shell execution, email delivery, and KV storage are host functions injected into plugins that need them. A plugin without the `llm_complete` import cannot call it — the function doesn't exist in the module's address space. Credentials (API keys, SMTP) live in Go closures; they are never written to WASM memory.

**Ephemeral lifecycle.** Each plugin is `NewPlugin` → `Call("execute")` → `Close` within a single Go function scope. Between steps, there is no process to attack, no memory to dump. An agent that doesn't exist can't be exploited.

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
│   ├── loop.go                     # DAG scheduler, parallel dispatch, splice
│   ├── hostfns.go                  # host function registry (dynamic, name-based)
│   ├── hostfns_shell.go            # exec_command: exec.Command + path/binary/metachar gates
│   ├── hostfns_memory.go           # kv_get/kv_put: NATS JetStream KV
│   ├── hostfns_sandbox.go          # sandbox_exec: runs code in a nested Wazero instance
│   ├── hostfns_email.go            # send_email: SMTP delivery via host fn (creds in closure)
│   ├── llm.go                      # llm_complete host function provider (mock / API / Ollama)
│   ├── registry.go                 # agent registry with enrichments
│   ├── builders.go                 # plan builders (including generic JSON-driven)
│   ├── builder_chat.go             # chat plan builder (memory → router → responder → memory)
│   └── taskstate.go                # HTTP handlers
│
├── cmd/webhook-gateway/            # lightweight HTTP gateway (chat message → task → poll)
│
├── pkg/dag/                        # DAG: dependency graph, ready-set, ancestors, splice
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

---

## Running the Demos

### Wasmclaw (Personal AI Assistant)

```bash
./examples/wasmclaw/run.sh                   # mock LLM (deterministic, no deps)
LLM_MODE=api ./examples/wasmclaw/run.sh      # NVIDIA NIM API (needs NV_API_KEY)
LLM_MODE=real ./examples/wasmclaw/run.sh     # local Ollama
```

### Fan-Out Summarizer

```bash
./examples/fan-out-summarizer/run.sh
```

### Prompt Injection

```bash
cd examples/prompt-injection && make demo    # requires Ollama (pulls model automatically)
```

### Manual Step-by-Step

```bash
cd components && cargo build --release && cd ..
go build -o ./bin/orchestrator ./provider/orchestrator/
nats-server -js &

OPA_POLICY=./examples/fan-out-summarizer \
OPA_DATA=./examples/fan-out-summarizer/data.json \
AGENT_REGISTRY_FILE=./examples/fan-out-summarizer/agents.json \
LLM_MODE=mock \
./bin/orchestrator &

curl -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "fan-out-summarizer",
    "query": "Compare these WebAssembly projects",
    "context": {"urls": "https://webassembly.org,https://wasmcloud.com,https://bytecodealliance.org"}
  }'

curl localhost:8080/tasks/<task-id> | jq .
```

---

## Configuration

### Orchestrator

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
| `EMAIL_ALLOWED_DOMAINS` | `example.com,partner-corp.com` | Comma-separated recipient domain allowlist for email-send host function |

### Webhook Gateway (`cmd/webhook-gateway/`)

| Variable | Default | Description |
|---|---|---|
| `LISTEN_ADDR` | `:8081` | Gateway HTTP server address |
| `ORCHESTRATOR_URL` | `http://localhost:8080` | Orchestrator API base URL |

### NVIDIA NIM API

The `run.sh` scripts map convenience variables to the orchestrator's LLM config:

| Variable | Maps to | Default |
|---|---|---|
| `NV_API_KEY` | `LLM_API_KEY` | — |
| `NV_MODEL` | `LLM_MODEL` | `nvdev/nvidia/llama-3.3-nemotron-super-49b-v1` |

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
