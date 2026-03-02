# Creating a New Agent

This guide covers two paths for adding an agent to WASM_AF:

- **Platform agent** (this page, sections 1-7) — a Rust crate checked into the repo, compiled at build time, with full access to host functions and a tailored OPA policy.
- **External agent (BYOA)** (section 8) — a pre-compiled `.wasm` binary uploaded to a running orchestrator via `POST /agents`, automatically sandboxed with the restrictive "untrusted" policy tier.

## Overview

Every agent in WASM_AF is a WASM module that exports a single `execute` function. The orchestrator creates a fresh plugin instance for each invocation, calls `execute`, reads the result, and destroys the instance.

```
TaskInput (JSON) → execute() → TaskOutput (JSON)
```

## 1. Create the Rust crate

From the `components/` directory:

```bash
cargo init --lib agents/my-agent
```

Add the crate to the workspace in `components/Cargo.toml`:

```toml
[workspace]
members = [
    "agent-types",
    "agents/my-agent",
    # ...existing members...
]
```

Set up `agents/my-agent/Cargo.toml`:

```toml
[package]
name = "my-agent"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib"]

[dependencies]
agent-types = { path = "../../agent-types" }
extism-pdk = "1.4"
serde = { version = "1", features = ["derive"] }
serde_json = "1"
```

The `crate-type = ["cdylib"]` is required for WASM compilation. The default target (`wasm32-unknown-unknown`) is set in `components/.cargo/config.toml`.

## 2. Implement the execute function

Create `agents/my-agent/src/lib.rs`:

```rust
use agent_types::{TaskInput, TaskOutput};
use extism_pdk::*;

#[derive(serde::Deserialize)]
struct MyRequest {
    query: String,
}

#[derive(serde::Serialize)]
struct MyResponse {
    result: String,
}

#[plugin_fn]
pub fn execute(Json(input): Json<TaskInput>) -> FnResult<Json<TaskOutput>> {
    let req: MyRequest = serde_json::from_str(&input.payload)
        .map_err(|e| Error::msg(format!("payload parse error: {e}")))?;

    if req.query.trim().is_empty() {
        return Err(Error::msg("query is required").into());
    }

    // Your agent logic here
    let response = MyResponse {
        result: format!("processed: {}", req.query),
    };

    let payload = serde_json::to_string(&response)
        .map_err(|e| Error::msg(format!("serialization error: {e}")))?;

    Ok(Json(TaskOutput {
        payload,
        metadata: vec![],
    }))
}
```

### Using host functions

If your agent needs LLM inference, import the shared types and declare the host function:

```rust
use agent_types::{LlmRequest, LlmMessage, LlmResponse, TaskInput, TaskOutput};
use extism_pdk::*;

#[host_fn]
extern "ExtismHost" {
    fn llm_complete(input: Json<LlmRequest>) -> Json<LlmResponse>;
}
```

Call it with `unsafe` (required by the FFI boundary):

```rust
let Json(resp) = unsafe {
    llm_complete(Json(llm_req))
        .map_err(|e| Error::msg(format!("LLM error: {e}")))?
};
```

Available host functions: `llm_complete`, `exec_command`, `kv_get`, `kv_put`, `sandbox_exec`, `send_email`.

### Using WASI filesystem

If your agent needs filesystem access (like `file-ops`), target `wasm32-wasip1` instead of `wasm32-unknown-unknown`. Use standard `std::fs` operations — Wazero's `AllowedPaths` enforces the boundary at runtime.

## 3. Register the agent

Add an entry to your example's `agents.json`:

```json
{
  "my-agent": {
    "wasm_name": "my_agent",
    "capability": "custom",
    "context_key": "my_agent_result",
    "host_functions": [],
    "payload_fields": {
      "query": "task.context.query"
    }
  }
}
```

Field reference:

| Field | Purpose |
|-------|---------|
| `wasm_name` | Filename stem of the `.wasm` binary (underscores, not hyphens) |
| `capability` | Category used in OPA policy rules (e.g., `http`, `llm`, `exec`, `kv`) |
| `context_key` | Key under which this agent's output appears in dependent steps' context |
| `host_functions` | List of host function names this agent needs (e.g., `["llm_complete"]`) |
| `payload_fields` | Maps agent input fields to sources: `step.params.<key>` or `task.context.<key>` |
| `enrichments` | Optional. Derived params (e.g., extract domain from URL) |
| `splice` | Optional. If `true`, the agent's output can insert new steps into the DAG |

## 4. Write the OPA policy

Add allow rules in your example's `policy.rego`:

```rego
import rego.v1

allow if {
    input.step.agent_type == "my-agent"
    input.agent.capability == "custom"
}
```

For agents with host functions or network access, add structured decisions:

```rego
host_functions := ["llm_complete"] if {
    input.step.agent_type == "my-agent"
}

allowed_hosts := ["api.example.com"] if {
    input.step.agent_type == "my-agent"
}
```

Write corresponding tests in `policy_test.rego`:

```rego
import rego.v1

test_my_agent_allowed if {
    allow with input as {
        "step": {"agent_type": "my-agent", "params": {}},
        "agent": {"capability": "custom"},
    }
}
```

Run them: `opa test examples/your-example/ -v`

## 5. Add to the build

In the Makefile or `run.sh`, add `-p my-agent` to the cargo build command:

```bash
cd components && cargo build --release -p my-agent
```

The output will be at `components/target/wasm32-unknown-unknown/release/my_agent.wasm`.

## 6. Wire into a plan builder

Either use the `generic` plan builder (pass steps as JSON in the task context) or create a custom Go plan builder in `provider/orchestrator/`. A plan builder returns `[]taskstate.Step` specifying execution order and dependencies.

## 7. Test

```bash
# Rust unit tests (native target)
cd components && cargo test --target "$(rustc -vV | grep host | awk '{print $2}')" -p my-agent

# OPA policy tests
opa test examples/your-example/ -v

# Integration test via curl
curl -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{"type":"generic","query":"test","context":{"steps":"[{\"agent_type\":\"my-agent\",\"params\":{\"query\":\"hello\"}}]"}}'
```

## 8. External agent (BYOA) — upload a pre-compiled WASM binary

If you have a pre-compiled `.wasm` binary (from any language — Rust, Go, C, Zig, AssemblyScript) that exports an `execute` function accepting `TaskInput` and returning `TaskOutput`, you can register it at runtime without rebuilding the orchestrator.

### Upload

```bash
curl -X POST localhost:8080/agents \
  -F 'meta={"name":"my-agent","context_key":"my_agent_result"}' \
  -F 'wasm=@path/to/my_agent.wasm'
```

The orchestrator will:
1. Validate the binary (instantiate with zero capabilities, check that `execute` exists).
2. Write it to `WASM_DIR/external/`.
3. Register it with `capability: "untrusted"`, `host_functions: []`, `external: true`.

The name must match `[A-Za-z0-9_-]+` and must not collide with a platform agent.

### Policy

External agents are governed by the BYOA policy tier (`policies/byoa.rego`). To use it, copy it alongside your existing `policy.rego` — since both share `package wasm_af.authz`, the rules merge automatically.

Default sandbox for untrusted agents:

| Constraint | Default | Configurable via |
|---|---|---|
| Host functions | none (`[]`) | — |
| Network (allowed_hosts) | none (`[]`) | — |
| Memory | 64 pages (4 MiB) | `data.config.byoa_max_memory_pages` |
| Timeout | 10 seconds | `data.config.byoa_timeout_sec` |
| Human approval | required | — |
| Execution gate | must be in approved list | `data.config.approved_external_agents` |

### Approve for execution

External agents are deny-by-default. Add the agent to the approved list in your `data.json`:

```json
{
  "config": {
    "approved_external_agents": ["my-agent"]
  }
}
```

Or update it at runtime via NATS KV (changes take effect immediately without restart):

```bash
nats kv put wasm-af-config approved-external-agents "my-agent,another-agent"
```

### Use in a task

Reference the agent by name in a plan step (e.g., via the `generic` plan builder or the splice mechanism):

```bash
curl -X POST localhost:8080/tasks \
  -H 'Content-Type: application/json' \
  -d '{
    "type": "generic",
    "query": "test",
    "context": {
      "steps": "[{\"agent_type\":\"my-agent\",\"params\":{\"query\":\"hello\"}}]"
    }
  }'
```

### List and remove

```bash
# List all agents (platform + external)
curl localhost:8080/agents | jq '.[] | select(.external)'

# Remove an external agent
curl -X DELETE localhost:8080/agents/my-agent
```

Platform agents cannot be removed via the API.

### Persistence

External agent registrations are persisted to NATS KV (`wasm-af-config/external-agents`). They survive orchestrator restarts and sync across replicas automatically.

---

## Key design principles

- **Validate inputs early.** Check for empty/invalid fields before calling host functions.
- **Use `map_err` consistently.** Prefix errors with context (e.g., `"payload parse error: {e}"`).
- **Never panic.** Use `Result`/`FnResult` for all fallible operations.
- **Minimize the trust boundary.** Prefer WASI filesystem over shell commands. Prefer OPA-injected config over ambient credentials.
- **Credentials stay in Go.** API keys should live in host function closures or OPA config injection, never in WASM memory.
