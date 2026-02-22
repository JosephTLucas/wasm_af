# wasm-af Architecture

## Overview

wasm-af is a WebAssembly agent framework built on [wasmCloud](https://wasmcloud.com).
Agents are Rust WASM components. Orchestration, LLM inference routing, and the
control plane are Go capability providers. All communication happens over NATS;
per-tenant isolation is enforced at the NATS network boundary (separate lattice
per tenant).

```
                       ┌─────────────────────────────────────────────┐
                       │              wasmCloud Lattice               │
                       │                                              │
  HTTP client  ──────► │  ┌─────────────┐    wRPC/NATS               │
  POST /tasks           │  │Orchestrator │◄──────────────────────┐   │
  GET  /tasks/{id}      │  │  Provider   │                        │   │
                       │  └──────┬──────┘                        │   │
                       │         │                                │   │
                       │  ctl API│ (wasmbus.ctl.v1.*)            │   │
                       │         ▼                                │   │
                       │  ┌─────────────┐   ┌─────────────┐      │   │
                       │  │ Policy Eng. │   │   JetStream  │      │   │
                       │  │  (WASM)     │   │  KV (Tasks)  │      │   │
                       │  └─────────────┘   └─────────────┘      │   │
                       │                                          │   │
                       │  ┌──────────────────────────────────┐   │   │
                       │  │  Agent Components (ephemeral)    │   │   │
                       │  │                                  │   │   │
                       │  │  ┌──────────┐  ┌────────────┐  │───┘   │
                       │  │  │web-search│  │ summarizer │  │        │
                       │  │  │  (WASM)  │  │   (WASM)   │  │        │
                       │  │  └────┬─────┘  └─────┬──────┘  │        │
                       │  └───────┼───────────────┼─────────┘        │
                       │          │               │                   │
                       │  ┌───────▼───┐   ┌───────▼─────┐           │
                       │  │http-client│   │llm-inference│           │
                       │  │ Provider  │   │  Provider   │           │
                       │  └───────────┘   └─────────────┘           │
                       └─────────────────────────────────────────────┘
```

## Component Roles

| Component | Language | Role |
|---|---|---|
| `policy-engine` | Rust WASM | Evaluates link-request rules; deny-by-default, first-match-wins |
| `web-search` | Rust WASM | Calls Brave Search API via `wasi:http/outgoing-handler` |
| `summarizer` | Rust WASM | Calls LLM via `wasm-af:llm/inference`; reads web-search results from context |
| `orchestrator` | Go provider | HTTP task API; drives policy → start → link → invoke → stop loop |
| `llm-inference` | Go provider | Routes `wasm-af:llm/inference.complete` to an OpenAI-compatible upstream |
| `http-client` | wasmCloud built-in | Satisfies `wasi:http/outgoing-handler` for agents making outgoing requests |

## Sequence Diagram: Research Task

```
Client        Orchestrator     Policy Engine    web-search      summarizer     LLM Provider
  │                │                 │               │               │               │
  │  POST /tasks   │                 │               │               │               │
  │───────────────►│                 │               │               │               │
  │                │                 │               │               │               │
  │ task_id        │  evaluate(      │               │               │               │
  │◄───────────────│    web-search   │               │               │               │
  │                │    → http-client│               │               │               │
  │                │    cap=http)    │               │               │               │
  │                │────────────────►│               │               │               │
  │                │  permit(mediated│               │               │               │
  │                │◄────────────────│               │               │               │
  │                │                 │               │               │               │
  │                │  StartComponent(web-search)     │               │               │
  │                │─────────────────────────────────►               │               │
  │                │  PutLink(web-search→http-client)│               │               │
  │                │─────────────────────────────────►               │               │
  │                │                 │               │               │               │
  │                │  execute(task-input)            │               │               │
  │                │────────────────────────────────►│               │               │
  │                │                 │               │  GET brave API│               │
  │                │                 │               │──────────────────────────────►│ (HTTP)
  │                │                 │               │◄──────────────────────────────│
  │                │  task-output    │               │               │               │
  │                │◄────────────────────────────────│               │               │
  │                │  StopComponent + DeleteLink     │               │               │
  │                │─────────────────────────────────►               │               │
  │                │                 │               │               │               │
  │                │  evaluate(summarizer→llm,cap=llm)               │               │
  │                │────────────────►│               │               │               │
  │                │  permit(mediated│               │               │               │
  │                │◄────────────────│               │               │               │
  │                │                 │               │               │               │
  │                │  StartComponent(summarizer)     │               │               │
  │                │──────────────────────────────────────────────►  │               │
  │                │  PutLink(summarizer→llm-inference)             │               │
  │                │──────────────────────────────────────────────►  │               │
  │                │                 │               │               │               │
  │                │  execute(task-input + context[web_search_results])              │
  │                │─────────────────────────────────────────────── ►│               │
  │                │                 │               │               │  complete(req) │
  │                │                 │               │               │──────────────►│
  │                │                 │               │               │◄──────────────│
  │                │  task-output    │               │               │               │
  │                │◄────────────────────────────────────────────────│               │
  │                │  StopComponent + DeleteLink                     │               │
  │                │──────────────────────────────────────────────►  │               │
  │                │                 │               │               │               │
  │  GET /tasks/id │                 │               │               │               │
  │───────────────►│                 │               │               │               │
  │  status=completed, results=...   │               │               │               │
  │◄───────────────│                 │               │               │               │
```

## Policy Engine

The policy engine is a Rust WASM component that implements `wasm-af:policy/evaluator`.
Rules are provided at evaluate-time via `wasi:config/runtime::get("policy-rules")`.

Rule evaluation is **deny-by-default** and **first-match-wins**:

```json
{
  "rules": [
    { "source": "wasm-af:web-search", "target": "wasm-af:summarizer",
      "capability": "agent-direct", "comms_mode": "direct" },
    { "source": "wasm-af:web-search", "target": "*",
      "capability": "http", "comms_mode": "mediated" },
    { "source": "wasm-af:summarizer", "target": "*",
      "capability": "llm", "comms_mode": "mediated" }
  ]
}
```

- `source` / `target` are agent type strings or `"*"` (wildcard).
- `capability`: `http`, `llm`, `kv`, `agent-direct`.
- `comms_mode`: `mediated` (orchestrator routes) or `direct` (runtime wRPC link between components).

Every evaluation decision is written to the audit log in JetStream (`wasm-af-audit` bucket).

## Comms Modes

| Mode | Who Routes | Use Case |
|---|---|---|
| `mediated` | Orchestrator | Standard capability access (HTTP, LLM). Orchestrator creates and tears down links per step. |
| `direct` | Component runtime | High-frequency agent↔agent calls. Orchestrator creates a static wRPC link between the two components; they call each other directly. |

## Multi-Tenancy

Each tenant runs in a separate wasmCloud lattice (separate NATS network prefix).
Isolation guarantee: the NATS network boundary prevents cross-tenant access.
The orchestrator is statically configured with the lattice prefix at startup.

## Data Flow

```
POST /tasks → build plan → for each step:
  1. policy.evaluate(source, target, capability)  → permit(comms_mode) | deny
  2. ctl.StartComponent(host, ociRef, componentID)
  3. ctl.PutLink(component → capability-provider)
  4. (if direct) ctl.PutLink(component → peer-component)
  5. wRPC execute(task-input{payload, context[]})  → task-output
  6. store.Update(task, step, results)
  7. ctl.DeleteLink(...)
  8. ctl.StopComponent(...)
```

## Task State Machine

```
pending → running → completed
                 ↘ failed
       (step) → denied  (policy blocked)
```

Task state is stored in JetStream KV (`wasm-af-tasks`). Per-step input/output
payloads are stored in `wasm-af-payloads`. Every audit event goes to `wasm-af-audit`.
