# Creating a New Agent

This guide walks through adding a new WASM agent plugin to WASM_AF, from Rust crate creation through OPA policy authoring and registration.

## Overview

Every agent in WASM_AF is a Rust crate compiled to WebAssembly that exports a single `execute` function. The orchestrator creates a fresh plugin instance for each invocation, calls `execute`, reads the result, and destroys the instance.

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

## Key design principles

- **Validate inputs early.** Check for empty/invalid fields before calling host functions.
- **Use `map_err` consistently.** Prefix errors with context (e.g., `"payload parse error: {e}"`).
- **Never panic.** Use `Result`/`FnResult` for all fallible operations.
- **Minimize the trust boundary.** Prefer WASI filesystem over shell commands. Prefer OPA-injected config over ambient credentials.
- **Credentials stay in Go.** API keys should live in host function closures or OPA config injection, never in WASM memory.
