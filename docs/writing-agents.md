# Writing wasm-af Agents

This guide walks through creating a new agent component from scratch.
Agents are Rust WASM components compiled for `wasm32-wasip2` and deployed to wasmCloud.

## Prerequisites

```bash
rustup target add wasm32-wasip2
cargo install wasm-tools       # for validation
wash --version                 # wasmCloud wash CLI
```

## Project Layout

```
components/agents/<your-agent>/
├── Cargo.toml
├── src/
│   └── lib.rs
└── wit/
    ├── world.wit
    └── deps/
        ├── wasm-af-agent/
        │   └── agent.wit      # copy of wit/wasm-af-agent.wit
        └── wasm-af-llm/       # include if your agent calls the LLM
            └── llm.wit        # copy of wit/wasm-af-llm.wit
```

Add your agent to `components/Cargo.toml`:

```toml
[workspace]
members = [
    "policy-engine",
    "agents/web-search",
    "agents/summarizer",
    "agents/<your-agent>",   # ← add this
]
```

## Step 1: Cargo.toml

```toml
[package]
name = "your-agent"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib"]

[dependencies]
# Default features are required — they enable the `macros` feature.
wit-bindgen = "0.53"
# Include the wasi crate if your agent makes outgoing HTTP requests.
wasi = "0.14"
serde = { version = "1", features = ["derive"] }
serde_json = "1"

[package.metadata.component]
package = "wasm-af:agent-your-agent"
```

> **Critical**: do **not** write `wit-bindgen = { version = "0.53", default-features = false }`.
> That disables the `macros` feature and the `generate!()` macro will be unavailable.

## Step 2: WIT World

```
wit/world.wit
```

```wit
package wasm-af:your-agent-impl@0.1.0;

world your-agent {
    // Shared types (task-input, task-output, agent-error).
    import wasm-af:agent/types@0.1.0;

    // Every agent must export this.
    export wasm-af:agent/handler@0.1.0;

    // Add capability imports as needed:
    // import wasi:config/runtime@0.2.0-draft;   // for wasmCloud config values
    // import wasm-af:llm/inference@0.1.0;        // for LLM access
}
```

### WIT deps

The WIT resolver needs local copies of any imported packages.
Copy them from the root `wit/` directory:

```bash
mkdir -p wit/deps/wasm-af-agent
cp ../../wit/wasm-af-agent.wit wit/deps/wasm-af-agent/agent.wit

# If you import wasm-af:llm:
mkdir -p wit/deps/wasm-af-llm
cp ../../wit/wasm-af-llm.wit wit/deps/wasm-af-llm/llm.wit

# If you import wasi:config:
mkdir -p wit/deps/wasi-config
cp ../policy-engine/wit/deps/wasi-config/runtime.wit wit/deps/wasi-config/
```

## Step 3: lib.rs Skeleton

```rust
wit_bindgen::generate!({
    path: "wit",
    world: "your-agent",
    generate_all,
});

// The ErrorCode enum lives in types, not in handler.
use exports::wasm_af::agent::handler::{AgentError, Guest, TaskInput, TaskOutput};
use crate::wasm_af::agent::types::ErrorCode;

// If you use wasi:config: note the `crate::` prefix to avoid ambiguity
// with the `wasi` crate (if also present).
// use crate::wasi::config::runtime as config;

struct Component;
export!(Component);

impl Guest for Component {
    fn execute(input: TaskInput) -> Result<TaskOutput, AgentError> {
        // Parse your payload shape.
        #[derive(serde::Deserialize)]
        struct MyInput { query: String }

        let req: MyInput = serde_json::from_str(&input.payload)
            .map_err(|e| AgentError {
                code: ErrorCode::InvalidInput,
                message: format!("payload parse error: {e}"),
            })?;

        // Do work...
        let result = format!("processed: {}", req.query);

        Ok(TaskOutput {
            payload: serde_json::json!({ "result": result }).to_string(),
            metadata: vec![],
        })
    }
}
```

### Module path cheat-sheet

| WIT entity | Rust path |
|---|---|
| `export wasm-af:agent/handler` (trait) | `exports::wasm_af::agent::handler::Guest` |
| `task-input`, `task-output` (types) | `exports::wasm_af::agent::handler::{TaskInput, TaskOutput}` |
| `error-code` enum | `crate::wasm_af::agent::types::ErrorCode` |
| `agent-error` record | `exports::wasm_af::agent::handler::AgentError` |
| `import wasm-af:llm/inference` (call) | `crate::wasm_af::llm::inference::complete(&req)` |
| `import wasi:config/runtime` (call) | `crate::wasi::config::runtime::get("key")` |
| `wasi` crate HTTP | `::wasi::http::outgoing_handler::handle(...)` |

> **`wasi` ambiguity**: `generate_all` creates a `crate::wasi` module for any
> `wasi:*` imports in your WIT world. If you also depend on the `wasi` crate,
> use `::wasi::` (double colon) for the crate and `crate::wasi::` for the generated bindings.

## Step 4: Build and Validate

```bash
# Build for WASI Preview 2.
cargo build --target wasm32-wasip2 --release -p your-agent

# Validate the component type.
wasm-tools validate target/wasm32-wasip2/release/your_agent.wasm

# Inspect the WIT world embedded in the binary.
wasm-tools component wit target/wasm32-wasip2/release/your_agent.wasm
```

The output of `wasm-tools component wit` should show:
- `export wasm-af:agent/handler@0.1.0` ✓
- Only the capability imports your agent actually uses ✓

## Step 5: Register the Agent with the Orchestrator

Add your agent type to the orchestrator config:

```bash
wash config put orchestrator-config \
    ... \
    "agent.your-agent=localhost:5000/wasm-af/your-agent:0.1.0"
```

Add a capability mapping in `provider/orchestrator/loop.go`:

```go
func capabilityForAgent(agentType string) PolicyCapability {
    switch agentType {
    case "web-search":
        return CapHTTP
    case "summarizer":
        return CapLLM
    case "your-agent":
        return CapHTTP  // or CapLLM, CapKV — whichever your agent needs
    default:
        return CapHTTP
    }
}
```

Add a context key mapping (so downstream agents can read your output):

```go
func contextKeyForAgent(agentType string) string {
    switch agentType {
    ...
    case "your-agent":
        return "your_agent_result"
    }
}
```

Add a step payload builder:

```go
func buildStepPayload(state *taskstate.TaskState, stepIdx int) string {
    step := &state.Plan[stepIdx]
    switch step.AgentType {
    ...
    case "your-agent":
        type p struct { Query string `json:"query"` }
        b, _ := json.Marshal(p{Query: state.Context["query"]})
        return string(b)
    }
}
```

## Step 6: Policy Rules

Add a rule to `deploy/policies/default.json` for your agent's capability:

```json
{
  "source": "wasm-af:your-agent",
  "target": "*",
  "capability": "http",
  "comms_mode": "mediated"
}
```

Then push the updated policy config:

```bash
POLICY=$(cat deploy/policies/default.json | tr -d '\n')
wash config put policy-rules-config "policy-rules=${POLICY}"
```

## Step 7: Task Plan

If your agent fits into the standard "research" task plan, edit
`provider/orchestrator/taskstate.go` → `buildPlan()` to include it.

Otherwise add a new task type and handle it in `buildPlan()`:

```go
case "your-workflow":
    return []taskstate.Step{
        newStep("your-agent", taskID, "step-1"),
        newStep("summarizer",  taskID, "step-2"),
    }, nil
```

## Reading Context from Prior Steps

If your agent needs output from a prior step, read the `input.context` slice:

```rust
let prior_output = input.context
    .iter()
    .find(|kv| kv.key == "web_search_results")
    .map(|kv| kv.val.as_str())
    .ok_or_else(|| AgentError {
        code: ErrorCode::InvalidInput,
        message: "web_search_results not in context".to_string(),
    })?;
```

The orchestrator populates `context` with the output of all preceding steps,
keyed by the agent type's canonical context key (see `contextKeyForAgent` above).

## Using WASI HTTP

For outgoing HTTP requests, depend on the `wasi` crate (not just wit-bindgen):

```toml
[dependencies]
wasi = "0.14"
```

Do **not** declare `wasi:http` in your WIT world — the `wasi` crate adds those
imports to the compiled binary automatically. The wasmCloud host will satisfy
them by linking your component to an `http-client` provider.

```rust
use ::wasi::http::outgoing_handler;
use ::wasi::http::types::{Fields, Method, OutgoingRequest, Scheme};
use ::wasi::io::poll::poll as wasi_poll;

let headers = Fields::new();
headers.append(&"Accept".to_string(), &b"application/json".to_vec()).unwrap();

let request = OutgoingRequest::new(headers);
request.set_method(&Method::Get).unwrap();
request.set_scheme(Some(&Scheme::Https)).unwrap();
request.set_authority(Some("api.example.com")).unwrap();
request.set_path_with_query(Some("/endpoint?q=hello")).unwrap();

let future = outgoing_handler::handle(request, None).unwrap();
let p = future.subscribe();
wasi_poll(&[&p]);
drop(p);

// FutureIncomingResponse::get() → Option<Result<Result<IncomingResponse, ErrorCode>, ()>>
// Three levels of unwrapping are required:
let response = future.get().unwrap().unwrap().unwrap();
```

## Using the LLM Inference Interface

Import `wasm-af:llm/inference` in your WIT world and call it:

```rust
// Bindings generated by wit-bindgen (not the wasi crate).
use crate::wasm_af::llm::inference::{self as llm, CompletionRequest, Message};

let resp = llm::complete(&CompletionRequest {
    model: "".to_string(),          // empty → provider picks its default
    messages: vec![
        Message { role: "system".to_string(), content: "You are helpful.".to_string() },
        Message { role: "user".to_string(),   content: user_prompt },
    ],
    max_tokens: 512,
    temperature: Some(0.3),
})?;

println!("LLM said: {}", resp.content);
```

The provider satisfying this import is `wasm-af:llm-inference`. Make sure the
WADM link exists: `your-agent` component → `llm-inference` provider,
namespace `wasm-af`, package `llm`, interfaces `[inference]`.
