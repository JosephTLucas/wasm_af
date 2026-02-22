#!/usr/bin/env bash
# deploy/setup.sh — create wasmCloud named configs and push component images
# before deploying the WADM application manifest.
#
# Prerequisites:
#   - wash installed and `wash up` running
#   - local OCI registry running on port 5000:
#       docker run -d -p 5000:5000 --name registry registry:2
#   - Brave Search API key (https://brave.com/search/api/)
#   - OpenAI-compatible LLM API key
#
# Usage:
#   export BRAVE_API_KEY="<key>"
#   export LLM_API_KEY="<openai-or-compatible-key>"
#   export LLM_BASE_URL="https://api.openai.com"          # optional
#   export LLM_DEFAULT_MODEL="gpt-4o-mini"                # optional
#   bash deploy/setup.sh

set -euo pipefail

REGISTRY="${REGISTRY:-localhost:5000}"
TAG="${TAG:-0.1.0}"

# ---------------------------------------------------------------------------
# 1. Build WASM components
# ---------------------------------------------------------------------------
echo "==> Building Rust WASM components..."
(cd components && cargo build --target wasm32-wasip2 --release \
    -p policy-engine -p web-search -p summarizer)

# ---------------------------------------------------------------------------
# 2. Build Go providers
# ---------------------------------------------------------------------------
echo "==> Building Go providers..."
GOARCH=amd64 GOOS=linux go build \
    -o /tmp/orchestrator ./provider/orchestrator/
GOARCH=amd64 GOOS=linux go build \
    -o /tmp/llm-inference ./provider/llm-inference/

# ---------------------------------------------------------------------------
# 3. Push to local OCI registry
# ---------------------------------------------------------------------------
echo "==> Pushing to local registry ${REGISTRY}..."

push_wasm() {
    local name="$1" src="$2"
    wash push \
        --insecure \
        "${REGISTRY}/wasm-af/${name}:${TAG}" \
        "${src}"
    echo "  pushed ${name}"
}

push_provider() {
    local name="$1" binary="$2"
    wash push \
        --insecure \
        "${REGISTRY}/wasm-af/${name}:${TAG}" \
        "${binary}" \
        --config-file /dev/null
    echo "  pushed provider ${name}"
}

push_wasm policy-engine \
    components/target/wasm32-wasip2/release/policy_engine.wasm
push_wasm web-search \
    components/target/wasm32-wasip2/release/web_search.wasm
push_wasm summarizer \
    components/target/wasm32-wasip2/release/summarizer.wasm

# Providers are pushed as par files (provider archive).
# Build par archives with wash par create if you have the tooling,
# otherwise reference the Go binary directly via a custom provider build.
# For simplicity in the PoC, we assume wash can handle raw binaries here.
# In production, use: wash par create --binary /tmp/<name> --vendor wasm-af ...
echo "  (provider push skipped — see docs for wash par create)"

# ---------------------------------------------------------------------------
# 4. Create wasmCloud named configs
# ---------------------------------------------------------------------------
echo "==> Creating named configs..."

: "${BRAVE_API_KEY:?BRAVE_API_KEY must be set}"
: "${LLM_API_KEY:?LLM_API_KEY must be set}"
LLM_BASE_URL="${LLM_BASE_URL:-https://api.openai.com}"
LLM_DEFAULT_MODEL="${LLM_DEFAULT_MODEL:-gpt-4o-mini}"

# Policy rules delivered to the policy-engine component via wasi:config/runtime.
POLICY_RULES=$(cat deploy/policies/default.json | tr -d '\n')
wash config put policy-rules-config \
    "policy-rules=${POLICY_RULES}"
echo "  policy-rules-config created"

# Orchestrator provider config: IDs it needs to call other components/providers.
# agent.<type>.allowed_hosts is passed as SourceConfig.allowed_hosts on every
# HTTP link the orchestrator creates for that agent type, ready for a
# wasi:http/outgoing-handler provider that enforces the allow-list.
wash config put orchestrator-config \
    policy_engine_id=policy-engine \
    llm_provider_id=llm-inference \
    http_provider_id=http-client \
    "agent.web-search=${REGISTRY}/wasm-af/web-search:${TAG}" \
    "agent.web-search.allowed_hosts=https://api.search.brave.com" \
    "agent.summarizer=${REGISTRY}/wasm-af/summarizer:${TAG}"
echo "  orchestrator-config created"

# Web-search component config (URL allow-list; brave_api_key is now a secret).
wash config put web-search-config \
    "url_allow_prefixes=https://api.search.brave.com"
echo "  web-search-config created"

# HTTP link policy: SourceConfig.allowed_hosts for the static WADM link.
# A wasi:http/outgoing-handler provider honouring this key will enforce the
# allow-list at the network level.  The current wasmcloud/http-client provider
# does not read this key; the agent enforces it at the application level.
wash config put web-search-http-policy \
    "allowed_hosts=https://api.search.brave.com"
echo "  web-search-http-policy created"

# LLM provider config (non-secret parts).
wash config put llm-config \
    llm_base_url="${LLM_BASE_URL}" \
    llm_default_model="${LLM_DEFAULT_MODEL}"
echo "  llm-config created"

# ---------------------------------------------------------------------------
# 5. Create named secrets (wasmCloud 1.x secret store)
# ---------------------------------------------------------------------------
echo "==> Creating named secrets..."

wash secret put llm-secrets \
    llm_api_key="${LLM_API_KEY}"
echo "  llm-secrets created"

wash secret put brave-secrets \
    brave_api_key="${BRAVE_API_KEY}"
echo "  brave-secrets created (wired to web-search component via WADM secrets stanza)"

# ---------------------------------------------------------------------------
# 6. Deploy the WADM application
# ---------------------------------------------------------------------------
echo "==> Deploying WADM application..."
wash app deploy deploy/wadm.yaml
echo "  application deployed"

echo ""
echo "Done. Check status with:"
echo "  wash app status wasm-af-demo"
echo ""
echo "Submit a task:"
echo "  curl -s -X POST http://localhost:8080/tasks \\"
echo "       -H 'Content-Type: application/json' \\"
echo "       -d '{\"type\":\"research\",\"context\":{\"query\":\"AI safety research 2025\"}}'"
