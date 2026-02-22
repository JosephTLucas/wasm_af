# WASM_AF — WebAssembly Agent Framework

**A security-first AI agent orchestration framework built on [wasmCloud](https://wasmcloud.com/) and the WebAssembly Component Model.**

WASM_AF leverages the stateless, composable, sandboxed nature of WebAssembly to create a performant, zero-trust AI agent runtime. Agents are WASM components — isolated by default, granted capabilities by policy, and destroyed when their work is done. No ambient authority. No lateral movement. No leaked secrets.

---

## Why WASM + AI Agents?

Today's agent frameworks treat security as an afterthought. Agents run in shared processes with broad network access, ambient credentials, and the ability to spawn arbitrary subprocesses. A prompt injection in one tool call can compromise the entire system.

WebAssembly components flip this model. A WASM component **cannot** touch the filesystem, network, or environment unless explicitly granted access through a typed interface. This isn't a policy layer bolted on top — it's a property of the execution model itself. WASM_AF is built on the premise that this is exactly the runtime model AI agents need.

wasmCloud, a CNCF incubating project, provides the orchestration layer: distributed hosting across a self-healing NATS-based lattice, declarative application manifests, runtime linking of capabilities, first-class secrets management, and sub-millisecond cold starts with scale-to-zero.

---

## Architecture

### Core Constraint: wasmCloud's Flat Hierarchy

wasmCloud enforces a **flat component hierarchy** by design — components cannot spawn other components. This is a deliberate zero-trust decision: in a secure environment, allowing workloads to create other workloads is a privilege escalation vector.

WASM_AF works *with* this constraint rather than against it. The orchestrator is **not** a WASM component. It is a **capability provider** — a long-lived, privileged process that sits outside the sandbox and manages agent lifecycles through wasmCloud's control interface API.

### The Three Layers

```
┌─────────────────────────────────────────────────────┐
│                   CONTROL PLANE                      │
│                                                      │
│  ┌─────────────┐  ┌──────────────┐  ┌────────────┐  │
│  │ Orchestrator │  │ Policy Engine│  │ Task State  │  │
│  │  (Provider)  │──│ (Component)  │  │ (NATS KV)  │  │
│  └──────┬───────┘  └──────────────┘  └────────────┘  │
│         │ control interface (NATS)                    │
├─────────┼───────────────────────────────────────────┤
│         ▼          AGENT PLANE                       │
│                                                      │
│  ┌───────────┐  ┌───────────┐  ┌───────────────┐    │
│  │ WebSearch  │  │ CodeExec   │  │ DataAnalysis  │   │
│  │  Agent     │  │  Agent     │  │   Agent       │   │
│  │ (Component)│  │ (Component)│  │  (Component)  │   │
│  └─────┬─────┘  └─────┬─────┘  └───────┬───────┘   │
│        │               │                │            │
├────────┼───────────────┼────────────────┼────────────┤
│        ▼               ▼                ▼            │
│                  CAPABILITY PLANE                    │
│                                                      │
│  ┌──────────┐ ┌───────────┐ ┌────────┐ ┌─────────┐  │
│  │HTTP Client│ │LLM Infer. │ │Key-Val │ │ Secrets │  │
│  │(Provider) │ │(Provider) │ │(Provid)│ │(Built-in│  │
│  └──────────┘ └───────────┘ └────────┘ └─────────┘  │
│                                                      │
└──────────────────────── NATS Lattice ───────────────┘
```

**Control Plane** — The orchestrator provider receives tasks, evaluates policy, and uses wasmCloud's control interface over NATS to start/stop agent components and manage their capability links. Task state (conversation context, accumulated results, remaining plan steps) lives in NATS JetStream KV, giving the orchestrator a persistent, distributed scratchpad and a built-in audit trail.

**Agent Plane** — Each agent is a stateless WASM component, typically compiled from Rust via `wasm32-wasip2`. Agents implement a known WIT interface, execute a focused task, return results, and are scaled to zero. They have no awareness of each other and no ability to spawn peers.

**Capability Plane** — Providers deliver concrete implementations of abstract capabilities (HTTP, key-value, LLM inference, etc.). The orchestrator controls which agents are linked to which providers, and with what configuration. This is where policy becomes enforcement.

---

## Core Principles

### 1. Policy-Driven Capability Grants

An agent does not declare what it needs — the **orchestrator decides what it gets**, constrained by policy.

When a task requires a web-search agent, the orchestrator:

1. Evaluates the policy engine (itself a WASM component — immutable, auditable, versioned via OCI)
2. Starts the `web-search-agent` component on an available host
3. Links it to the `http-client` provider **scoped to an allow-list of domains**
4. Injects only the API keys relevant to that specific link via wasmCloud's secrets backend
5. On task completion, scales the component to zero and removes the links

The agent component never sees a configuration file. It calls `wasi:http/outgoing-handler` and the provider enforces the boundary. The allow-list isn't advisory — it's the only network the agent can reach.

### 2. Secrets Isolation by Construction

wasmCloud's secrets model (v1.1+) scopes secrets to specific **links** between components and providers. A web-search agent receives its API key through its link to the HTTP provider. It cannot access the database password scoped to a different agent's link to the Postgres provider — there is no interface, no function, no path to reach it.

Secrets are:
- Retrieved by the host from a pluggable backend (Vault, NATS KV, Kubernetes Secrets)
- Cached in-memory alongside the component
- Zeroed from memory when the component stops
- Never transmitted over the wire between components

This eliminates the "credential manager" as a separate component. The wasmCloud host *is* the credential manager, and link-scoped secrets *are* the access control policy.

### 3. Zero-Trust Inter-Agent Communication

Agents do not talk to each other directly. All inter-agent communication is mediated by the orchestrator provider.

If a research task decomposes into `web-search → summarize → draft`, the orchestrator:

1. Runs `web-search-agent`, collects results
2. Stores intermediate state in NATS KV
3. Runs `summarize-agent` with those results as input
4. Stores the summary
5. Runs `draft-agent` with the summary and original task context

Each agent sees only its own inputs and outputs. No agent has a handle to another agent. The orchestrator is the only entity with lattice-wide visibility.

For latency-sensitive pipelines, the orchestrator can establish **pre-approved direct links** between specific component pairs at deploy time — but this is an explicit policy decision, not a default.

### 4. LLM Access as a Capability

LLM inference is modeled as a standard wasmCloud capability, delivered by a dedicated provider.

```wit
// wit/wasm-af-llm.wit
package wasm-af:llm@0.1.0;

interface inference {
    record completion-request {
        model: string,
        messages: list<message>,
        max-tokens: u32,
        temperature: option<f32>,
    }

    record message {
        role: string,
        content: string,
    }

    record completion-response {
        content: string,
        usage: token-usage,
    }

    record token-usage {
        prompt-tokens: u32,
        completion-tokens: u32,
    }

    complete: func(req: completion-request) -> result<completion-response, string>;
}
```

The `llm-inference` provider is a long-lived process that holds connection pools to model endpoints, enforces token budgets per agent, and provides a natural audit point. Agents call `wasm-af:llm/inference.complete` — they don't know or care whether the backing model is a local deployment, an API, or a routing layer across multiple providers.

### 5. Lifecycle as a Security Primitive

WASM components start in sub-milliseconds and scale to zero with no cold-start penalty. WASM_AF treats this as a security feature, not just a performance one.

An agent that doesn't exist can't be exploited. Between invocations, there is no running process to attack, no memory to dump, no socket to probe. The orchestrator spins up exactly the components needed for a task, for exactly as long as they're needed, and then they're gone — along with their in-memory secrets.

---

## Project Structure

WASM_AF is a **Go project** at the root. The framework *is* the orchestrator and the provider infrastructure — that's what ships. Rust components (agents, policy engine) are workloads that run *on* the framework, closer to plugins or examples than to the framework itself.

### Language Split

**Go for providers** (orchestrator, LLM inference). Providers are native binaries, not WASM. They manage long-lived processes, connection pools, and control-plane coordination over NATS. Go gives us mature NATS client libraries (NATS is written in Go), wasmCloud's `provider-sdk-go` (invested heavily since v1.1), a strong concurrency model for managing simultaneous agent lifecycles, and fast compile times for rapid iteration on what is fundamentally a coordination problem.

**Rust for components** (agents, policy engine). These compile to WASM via `wasm32-wasip2`. Rust's target support is the most mature in the ecosystem, produces tiny binaries (kilobytes), and has first-class `wit-bindgen` support. For security-critical code like the policy engine, the type system catches errors that would be runtime bugs in other languages.

The language boundary doubles as an architectural boundary: if you see Go, you're in the control/capability plane; if you see Rust, you're in the agent plane.

### Repository Layout

```
wasm-af/
├── go.mod                        # root Go module
├── go.sum
├── README.md
│
├── cmd/
│   └── wasm-af/                  # CLI entrypoint (if needed)
│
├── provider/
│   ├── orchestrator/             # core framework — the agent lifecycle manager
│   │   ├── main.go
│   │   ├── controlplane.go       # wasmCloud control interface (NATS)
│   │   ├── taskstate.go          # NATS JetStream KV state management
│   │   ├── policy.go             # policy engine client
│   │   └── loop.go               # plan → dispatch → collect → iterate
│   └── llm-inference/            # LLM capability provider
│       ├── main.go
│       ├── router.go             # model endpoint routing
│       └── budget.go             # per-agent token accounting
│
├── pkg/
│   ├── controlplane/             # wasmCloud control interface helpers
│   ├── taskstate/                # NATS KV task state types and operations
│   └── policy/                   # policy evaluation client types
│
├── wit/                          # WIT interface definitions (shared contract layer)
│   ├── wasm-af-agent.wit         # agent ↔ orchestrator interface
│   ├── wasm-af-llm.wit           # agent ↔ LLM provider interface
│   └── wasm-af-policy.wit        # orchestrator ↔ policy engine interface
│
├── components/                   # Rust workspace — workloads, not framework
│   ├── Cargo.toml                # workspace root
│   ├── policy-engine/            # policy evaluation component (WASM)
│   │   ├── Cargo.toml
│   │   └── src/
│   │       └── lib.rs
│   └── agents/                   # example agent components (WASM)
│       ├── web-search/
│       │   ├── Cargo.toml
│       │   └── src/
│       │       └── lib.rs
│       └── summarizer/
│           ├── Cargo.toml
│           └── src/
│               └── lib.rs
│
├── deploy/
│   ├── wadm.yaml                 # wasmCloud application manifest
│   └── policies/                 # example policy definitions
│
└── docs/
    ├── architecture.md
    └── writing-agents.md
```

The `wit/` directory is the contract layer between the two worlds. Go providers implement one side, Rust components implement the other, and `wit-bindgen` / `wrpc` handle the glue. It lives at the root because it belongs to neither language — it's the shared API.

As the project matures, the example agents in `components/agents/` should migrate to a separate repository. They demonstrate how to build *on* WASM_AF, not part of the framework itself. For the PoC, a single repo keeps iteration fast.

---

## Proof of Concept

The initial PoC targets three pieces that together demonstrate the core thesis:

**1. Orchestrator Provider** (`provider/orchestrator/`, Go)

The framework's core. Exposes an HTTP endpoint for task submission, implements the agent loop (plan → dispatch → collect → iterate), uses `wasmbus.ctl.v1.*` NATS subjects to start/stop components and manage links, and stores task state and agent results in NATS JetStream KV.

**2. Policy Engine** (`components/policy-engine/`, Rust → WASM)

Evaluates whether a requested capability link is permitted. Consulted by the orchestrator before every link creation. Immutable once deployed; new versions are published to OCI and rolled forward. Potential OPA integration for organizations with existing Rego policies.

**3. Web Search Agent** (`components/agents/web-search/`, Rust → WASM)

Accepts a query via its exported WIT interface. Calls `wasi:http/outgoing-handler` to reach permitted search APIs. Returns structured results to the orchestrator. Has no other capabilities, no state, no secrets beyond its scoped API key.

This demonstrates sandboxed agents with policy-driven capability grants, secrets isolation, dynamic lifecycle management, and zero-trust communication — all leveraging wasmCloud primitives rather than reinventing them.

---

## What wasmCloud Gives Us (and What We Build)

| Concern | wasmCloud Primitive | WASM_AF Builds |
|---|---|---|
| Agent sandboxing | Component Model (WASI 0.2) | — (free) |
| Capability grants | Runtime linking | Policy engine governing link creation |
| Secrets isolation | Link-scoped secrets + pluggable backends | Per-agent secret policies |
| Agent lifecycle | Scale-to-zero, sub-ms cold starts | Orchestrator-driven create/destroy loop |
| Distributed comms | NATS lattice + wRPC | Mediated inter-agent message routing |
| State management | NATS JetStream KV | Task context and audit trail schema |
| Observability | OpenTelemetry integration | Agent-level tracing and token accounting |
| LLM access | Custom capability provider | `wasm-af:llm` WIT interface + provider |
| Deployment | WADM manifests + OCI registries | Agent and policy component registry |

---

## Relationship to Existing Agent Frameworks

WASM_AF is a standalone project. It is not a plugin, extension, or PR target for existing agent toolkits.

Frameworks like NVIDIA NeMo Agent Toolkit, LangChain, CrewAI, and similar libraries operate at a different layer. They are Python libraries for wiring together agents that run as functions in shared processes with full ambient system access — network, filesystem, environment variables, arbitrary subprocess spawning. Their value is in connecting, profiling, and optimizing agents you've already built. WASM_AF's value is in ensuring agents *cannot access anything they haven't been explicitly granted*.

These are different execution models, not different flavors of the same idea. An agent framework that assumes agents are Python functions with ambient authority is architecturally incompatible with a runtime whose foundational premise is that agents have zero ambient authority.

**Where interoperability makes sense (future):** Frameworks like NeMo Agent Toolkit support the A2A (Agent-to-Agent) protocol. A mature WASM_AF could expose its orchestrator as an A2A server, allowing external frameworks to delegate tasks to WASM_AF's sandboxed agent plane when they need hardened execution guarantees. The external framework handles high-level coordination and decides *what* to do; WASM_AF handles *how* to do it safely. This is a v2 integration story, not a starting point.

---

## Open Questions

- **Agent-to-agent streaming.** For long-running LLM generations, should agents stream tokens through the orchestrator, or should we allow direct wRPC links between producer/consumer component pairs under policy?
- **Recursive decomposition.** When a subagent determines it needs *another* subagent, the request must round-trip through the orchestrator. Is the latency cost acceptable, or do we need a "pre-approved plan" model where the orchestrator sets up an entire pipeline ahead of time?
- **Multi-tenancy.** wasmCloud lattices provide namespace isolation. Should each tenant get its own lattice, or can we share a lattice with link-level isolation?
- **Component composition vs. runtime linking.** wasmCloud supports both build-time composition (merging components into one binary) and runtime composition via wRPC. Which is the right default for agent pipelines?
- **Token budgeting.** The LLM provider is the natural enforcement point for per-task token limits. How should budgets propagate when a task fans out into multiple subagent invocations?

---

## Getting Started

> 🚧 **Status: Design phase.** This README is the specification. Implementation has not begun.

Prerequisites:
- [Go](https://go.dev/) 1.22+ (providers)
- [Rust](https://rustup.rs/) with `wasm32-wasip2` target (components)
- [wasmCloud](https://wasmcloud.com/docs/installation/) (`wash` CLI)
- [NATS](https://nats.io/) server (bundled with `wash up`)

---

## License

TBD

---

*WASM_AF is not affiliated with the wasmCloud project. wasmCloud is a CNCF incubating project.*
