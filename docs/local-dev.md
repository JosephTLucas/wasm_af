# Local Development Guide

This guide runs the full wasm-af stack on your laptop with no cloud accounts,
no Docker, and (optionally) no search API key.

## What you need

| Tool | Purpose | Install |
|---|---|---|
| `wash` | wasmCloud CLI — starts the runtime, deploys apps | `cargo install wash-cli` or [releases](https://github.com/wasmCloud/wasmCloud/releases) |
| Rust + `wasm32-wasip2` target | Build agent components | `rustup target add wasm32-wasip2` |
| Go ≥ 1.21 | Build orchestrator and LLM inference providers | [go.dev](https://go.dev/dl/) |
| An LLM API key | The summarizer calls an OpenAI-compatible endpoint | See below |

That is the complete list. Docker is **not required** for local development.

---

## Why no Docker?

The production manifest (`deploy/wadm.yaml`) references components by OCI image
(`localhost:5000/wasm-af/...`), which requires a registry to push to and pull
from — that is what the Docker registry container provides.

The local development manifest (`deploy/wadm-local.yaml`) uses `file://` URIs
instead, pointing directly at the compiled `.wasm` binaries on disk. wasmCloud
reads them without any registry. The flow is simply:

```
cargo build → .wasm file on disk → wasmCloud reads it directly
```

---

## What is Brave Search?

Brave Search offers a REST API for web search — the web-search agent calls it to
fetch real results. It has a free tier (2 000 queries / month) and does not
require a credit card.

Sign up: <https://brave.com/search/api/>

**You do not need a Brave API key for local development.** The web-search agent
has a `mock_results` config flag that returns hard-coded stub results so you can
run the full pipeline end-to-end without any search account. The summarizer will
still call the real LLM to produce a summary of those stubs.

If you want real search results later, set `mock_results=false` and supply the key.

---

## Step-by-step local startup

### 1. Build everything

```bash
# Rust WASM components
cargo build --target wasm32-wasip2 --release \
    -p policy-engine -p web-search -p summarizer

# Go providers (run as native binaries, not WASM)
go build -o /tmp/orchestrator   ./provider/orchestrator/
go build -o /tmp/llm-inference  ./provider/llm-inference/
```

### 2. Start the wasmCloud runtime

```bash
wash up --detached
# Starts an embedded NATS server on 4222 and a wasmCloud host.
# Check it's running:
wash get hosts
```

### 3. Create named configs

wasmCloud passes config to components via named config objects. Create them
before deploying the application.

```bash
# Policy rules — controls which agent pairs are allowed and how they communicate.
POLICY=$(cat deploy/policies/default.json | tr -d '\n')
wash config put policy-rules-config "policy-rules=${POLICY}"

# Web-search — mock mode ON means no Brave API key is needed.
wash config put web-search-config \
    mock_results=true \
    url_allow_prefixes=""

# LLM config — point at any OpenAI-compatible endpoint.
wash config put llm-config \
    llm_base_url="https://api.openai.com" \
    llm_default_model="gpt-4o-mini"

# Orchestrator — tells it where to find the policy engine and providers.
# These names must match the component/provider names in the WADM manifest.
wash config put orchestrator-config \
    policy_engine_id=policy-engine \
    llm_provider_id=llm-inference \
    http_provider_id=http-client \
    "agent.web-search=file://./components/target/wasm32-wasip2/release/web_search.wasm" \
    "agent.summarizer=file://./components/target/wasm32-wasip2/release/summarizer.wasm"
```

### 4. Create secrets

wasmCloud secrets are separate from config (they are encrypted at rest).

```bash
# Required: your LLM API key (OpenAI, Anthropic, or any compatible provider).
wash secret put llm-secrets llm_api_key="sk-..."

# Optional: Brave Search API key (not needed if mock_results=true).
# wash secret put brave-secrets brave_api_key="BSA..."
```

### 5. Start the Go providers

The orchestrator and LLM inference providers are Go binaries. For local dev,
start them with `wash provider start` (which runs them as child processes of
the wasmCloud host):

```bash
# The providers read their host data from stdin when launched by wash.
# Use wash's provider development mode:
wash provider start \
    --provider-id orchestrator \
    /tmp/orchestrator

wash provider start \
    --provider-id llm-inference \
    /tmp/llm-inference
```

> **Note**: wasmCloud's provider packaging format is a `.par` (provider archive)
> file for production. For local dev, `wash provider start` can accept a raw
> binary. If you hit issues, see the [provider archive section](#building-provider-archives) below.

### 6. Deploy the application

```bash
# Use the local manifest — components are loaded from file:// paths.
wash app deploy deploy/wadm-local.yaml

# Check everything is running:
wash app status wasm-af-demo
wash get components
```

### 7. Submit a task

```bash
curl -s -X POST http://localhost:8080/tasks \
     -H 'Content-Type: application/json' \
     -d '{"type":"research","context":{"query":"wasmCloud architecture"}}' \
  | jq .
```

Poll for the result:

```bash
TASK_ID="<id from above>"
watch -n 2 "curl -s http://localhost:8080/tasks/${TASK_ID} | jq '{status,plan:.plan[].status}'"
```

---

## Switching to real search results

When you are ready to test with live search data:

```bash
# 1. Get a free Brave API key at https://brave.com/search/api/
# 2. Store it as a secret:
wash secret put brave-secrets brave_api_key="BSA..."

# 3. Update web-search config to disable mock mode and reference the secret.
#    (The secret must be wired into the web-search component's config link —
#     see deploy/wadm.yaml for the secrets: block pattern.)
wash config put web-search-config \
    mock_results=false \
    url_allow_prefixes=""
```

Alternatively, any search API that accepts the same request shape
(`GET /...?q=<query>` → JSON with `web.results[].{title,url,description}`)
works with the existing web-search agent code.

---

## Teardown

```bash
wash app delete wasm-af-demo
wash down
```

---

## Building Provider Archives

For production (or if `wash provider start <binary>` is not supported in your
version), providers must be packaged as `.par` files:

```bash
# Build for the host architecture.
go build -o /tmp/orchestrator ./provider/orchestrator/

# Create a signed provider archive.
wash par create \
    --binary /tmp/orchestrator \
    --vendor wasm-af \
    --name orchestrator \
    --version 0.1.0 \
    --output /tmp/orchestrator.par.gz

# Push to a local registry (then use the OCI ref in wadm.yaml).
wash push --insecure localhost:5000/wasm-af/orchestrator:0.1.0 /tmp/orchestrator.par.gz
```

If you do use a local registry, start it with:

```bash
# Single Docker command — only needed for the registry, not for wasmCloud itself.
docker run -d -p 5000:5000 --name registry registry:2
```

---

## Choosing a different LLM

The summarizer talks to any endpoint that implements the OpenAI chat completions
API (`POST /v1/chat/completions`). Set `llm_base_url` to point at it:

| Provider | `llm_base_url` | `llm_default_model` |
|---|---|---|
| OpenAI | `https://api.openai.com` | `gpt-4o-mini` |
| Anthropic (via proxy) | your proxy URL | `claude-haiku-4-5-20251001` |
| Ollama (local, no key) | `http://localhost:11434` | `llama3.2` |
| LM Studio (local, no key) | `http://localhost:1234` | (your loaded model) |

For a **fully local, zero-API-key** setup: run Ollama, set `llm_base_url` to
`http://localhost:11434`, set `llm_api_key` to any non-empty string (Ollama
ignores it), and set `mock_results=true` for the search.

```bash
# Install Ollama: https://ollama.com
ollama pull llama3.2

wash config put llm-config \
    llm_base_url="http://localhost:11434" \
    llm_default_model="llama3.2"

wash secret put llm-secrets llm_api_key="local"
```
