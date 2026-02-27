#!/usr/bin/env bash
# Fan-Out Summarizer — end-to-end demo
#
# Builds everything, starts NATS, runs the orchestrator, and demonstrates:
#   - per-instance capability scoping (each WASM sandbox gets exactly one domain)
#   - policy gating (OPA/Rego deny-by-default, data-driven domain checks)
#   - physical capability isolation (missing imports, not advisory rules)
#   - live allow-list enforcement (NATS KV update, no restart)
#
# Prerequisites: rust (with wasm32-unknown-unknown target), go, jq
# Optional: nats-server OR wash CLI (for bundled NATS), nats CLI (for live KV demo)
#
# Usage:
#   ./examples/fan-out-summarizer/run.sh
#   URLS="https://a.com,https://b.com" ./examples/fan-out-summarizer/run.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

[ -f "$HOME/.cargo/env" ] && . "$HOME/.cargo/env"

URLS="${URLS:-https://webassembly.org,https://wasmcloud.com,https://bytecodealliance.org}"
QUERY="${QUERY:-Compare these WebAssembly ecosystem projects}"
ORCH_PID=""
NATS_PID=""
NATS_STARTED_BY_US=""
NATS_STORE_DIR=""

cleanup() {
    [ -n "$ORCH_PID" ] && kill "$ORCH_PID" 2>/dev/null || true
    if [ -n "$NATS_STARTED_BY_US" ] && [ -n "$NATS_PID" ]; then
        kill "$NATS_PID" 2>/dev/null || true
        [ -n "$NATS_STORE_DIR" ] && rm -rf "$NATS_STORE_DIR" 2>/dev/null || true
    fi
}
trap cleanup EXIT

die() { echo "  ERROR: $1" >&2; exit 1; }

extract_host() {
    echo "$1" \
        | sed -E 's#^[a-zA-Z][a-zA-Z0-9+.-]*://##' \
        | sed -E 's#/.*$##' \
        | sed -E 's#:.*$##'
}

echo ""
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║          WASM_AF — Fan-Out Summarizer Demo          ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""

# ── 1. Build ──────────────────────────────────────────────────────────────────
echo "  [1/4] Building..."

if ! rustup target list --installed 2>/dev/null | grep -q wasm32-unknown-unknown; then
    echo "        Adding wasm32-unknown-unknown target..."
    rustup target add wasm32-unknown-unknown || die "Failed to add wasm32-unknown-unknown target."
fi

(cd components && cargo build --release 2>&1) || die "Rust build failed."
go build -o ./bin/orchestrator ./provider/orchestrator/ 2>&1 || die "Go build failed."
echo "        Done."
echo ""

# ── 2. Start NATS ─────────────────────────────────────────────────────────────
echo "  [2/4] Starting NATS..."

if lsof -i :4222 -sTCP:LISTEN >/dev/null 2>&1; then
    echo "        NATS already running on :4222"
else
    NATS_BIN=""
    if command -v nats-server >/dev/null 2>&1; then
        NATS_BIN="nats-server"
    elif [ -x "$HOME/.local/share/wash/downloads/nats-server" ]; then
        NATS_BIN="$HOME/.local/share/wash/downloads/nats-server"
    else
        die "nats-server not found. Install via: brew install nats-server  OR  run 'wash up' once to download it."
    fi

    NATS_STORE_DIR="$(mktemp -d /tmp/wasm-af-nats-XXXXXX)"
    $NATS_BIN -js -sd "$NATS_STORE_DIR" -p 4222 > /tmp/wasm-af-nats.log 2>&1 &
    NATS_PID=$!
    NATS_STARTED_BY_US="1"
    sleep 2

    if ! kill -0 "$NATS_PID" 2>/dev/null; then
        cat /tmp/wasm-af-nats.log
        die "NATS failed to start."
    fi
    echo "        NATS started (PID $NATS_PID, store: $NATS_STORE_DIR)"
fi
echo ""

# ── 3. Start orchestrator ─────────────────────────────────────────────────────
echo "  [3/4] Starting orchestrator..."

if lsof -ti:8080 >/dev/null 2>&1; then
    echo "        Stopping stale process on :8080..."
    lsof -ti:8080 | xargs kill 2>/dev/null || true
    sleep 1
fi

OPA_POLICY="$ROOT/examples/fan-out-summarizer" \
OPA_DATA="$ROOT/examples/fan-out-summarizer/data.json" \
AGENT_REGISTRY_FILE="$ROOT/examples/fan-out-summarizer/agents.json" \
LLM_MODE=mock \
WASM_DIR="$ROOT/components/target/wasm32-unknown-unknown/release" \
    ./bin/orchestrator > /tmp/wasm-af-orchestrator.log 2>&1 &
ORCH_PID=$!
sleep 2

if ! curl -sf http://localhost:8080/healthz > /dev/null 2>&1; then
    cat /tmp/wasm-af-orchestrator.log
    die "Orchestrator failed to start."
fi
echo "        Orchestrator running (PID $ORCH_PID)"

# Seed the NATS KV allowed-fetch-domains so that a stale value from a previous
# example run (e.g. prompt-injection) does not override this example's data.json.
if command -v nats >/dev/null 2>&1; then
    nats kv put wasm-af-config allowed-fetch-domains \
        "webassembly.org,wasmcloud.com,bytecodealliance.org" > /dev/null 2>&1
    sleep 1
fi
echo ""

# ── 4. Demo ───────────────────────────────────────────────────────────────────
echo "  [4/4] Running demo..."
echo ""

# ── Submission policy gate ────────────────────────────────────────────────────
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        SUBMISSION POLICY GATE (OPA)                  ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  Submitting type='forbidden-task' (not in allowed_task_types)..."
DENY_BODY=$(curl -s -w "\n%{http_code}" -X POST http://localhost:8080/tasks \
    -H "Content-Type: application/json" \
    -d '{"type":"forbidden-task","query":"this should be rejected"}')
DENY_HTTP=$(echo "$DENY_BODY" | tail -1)
DENY_MSG=$(echo "$DENY_BODY" | head -1)
echo "    HTTP status: $DENY_HTTP"
echo "    Response:    $DENY_MSG"
if [ "$DENY_HTTP" = "403" ]; then
    echo "    ✓ Rejected before plan is built."
fi
echo ""

echo "  Now submitting type='fan-out-summarizer' (allowed)..."
echo ""
echo "        Query:  $QUERY"
echo "        URLs:"
IFS=',' read -ra URL_ARRAY <<< "$URLS"
for u in "${URL_ARRAY[@]}"; do
    echo "          - $(echo "$u" | xargs)"
done
echo ""

TASK_ID=$(curl -sf -X POST http://localhost:8080/tasks \
    -H "Content-Type: application/json" \
    -d "$(jq -n \
        --arg type "fan-out-summarizer" \
        --arg query "$QUERY" \
        --arg urls "$URLS" \
        '{type: $type, query: $query, context: {urls: $urls}}')" \
    | jq -r '.task_id')

[ -z "$TASK_ID" ] || [ "$TASK_ID" = "null" ] && die "Failed to submit task."
echo "        Task ID: $TASK_ID"

# Poll until complete
printf "        Waiting..."
STATUS="unknown"
STATE=""
for _ in $(seq 1 30); do
    STATE=$(curl -sf "http://localhost:8080/tasks/${TASK_ID}" || echo '{}')
    STATUS=$(echo "$STATE" | jq -r '.status // "unknown"')
    [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ] && break
    printf "."
    sleep 2
done
echo ""
echo ""

[ "$STATUS" = "failed" ] && { echo "  Task FAILED:"; echo "$STATE" | jq -r '.error'; exit 1; }
[ "$STATUS" != "completed" ] && die "Task did not complete within 60 seconds (status: $STATUS)"

# ── Capability assignments ─────────────────────────────────────────────────────
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║           SANDBOX CAPABILITY ASSIGNMENTS            ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
NUM_FETCHES=$(echo "$STATE" | jq '[.plan[] | select(.agent_type == "url-fetch")] | length')
echo "$STATE" | jq -r '
    .plan[] | select(.agent_type == "url-fetch") |
    "    url-fetch   HTTP → \(.params.url // "n/a")     LLM: absent     [\(.status)]"'
echo "$STATE" | jq -r '
    .plan[] | select(.agent_type == "summarizer") |
    "    summarizer  HTTP → (none)                       LLM: injected   [\(.status)]"'
echo ""

# ── Cross-instance isolation proof ────────────────────────────────────────────
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        CROSS-INSTANCE ISOLATION                     ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""

if [ "${#URL_ARRAY[@]}" -lt 2 ]; then
    echo "  (Only one url-fetch step in the plan — need at least two domains to demonstrate.)"
    echo "  Run with URLS containing 2+ domains to see this section."
else
    FIRST_URL="$(echo "${URL_ARRAY[0]}" | xargs)"
    SECOND_URL="$(echo "${URL_ARRAY[1]}" | xargs)"
    FIRST_DOMAIN="$(extract_host "$FIRST_URL")"
    SECOND_DOMAIN="$(extract_host "$SECOND_URL")"
    if [ -z "$FIRST_DOMAIN" ] || [ -z "$SECOND_DOMAIN" ]; then
        echo "  Unable to extract domains from URLS."
        echo "  URLS must include absolute URLs like https://example.com/path"
        echo ""
    else
    PROBE_ID=$(curl -sf -X POST http://localhost:8080/tasks \
        -H "Content-Type: application/json" \
        -d "$(jq -n \
            --arg restricted_to "$FIRST_DOMAIN" \
            --arg fetch_url "$SECOND_URL" \
            '{type:"isolation-test", query:"cross-instance isolation probe",
              context:{restricted_to:$restricted_to, fetch_url:$fetch_url}}')" \
        | jq -r '.task_id')

    if [ -z "$PROBE_ID" ] || [ "$PROBE_ID" = "null" ]; then
        echo "  ERROR: task submission failed"
    else
        for _ in $(seq 1 15); do
            PROBE_STATE=$(curl -sf "http://localhost:8080/tasks/${PROBE_ID}" || echo '{}')
            PROBE_STATUS=$(echo "$PROBE_STATE" | jq -r '.status // "unknown"')
            [ "$PROBE_STATUS" = "completed" ] || [ "$PROBE_STATUS" = "failed" ] && break
            sleep 2
        done
        STEP_STATUS=$(echo "$PROBE_STATE" | jq -r '.plan[0].status // "unknown"')
        STEP_ERR=$(echo "$PROBE_STATE" | jq -r '.plan[0].error // ""')
        echo "    instance allowed_hosts: [$FIRST_DOMAIN]"
        echo "    attempted fetch:        $SECOND_URL"
        echo "    step result:            $STEP_STATUS"
        [ -n "$STEP_ERR" ] && echo "    error:                  $(echo "$STEP_ERR" | head -c 120)"
        echo ""
        STEP_ERR_LC=$(echo "$STEP_ERR" | tr '[:upper:]' '[:lower:]')
        if [ "$STEP_STATUS" = "failed" ] && ! echo "$STEP_ERR_LC" | grep -q "no rule permits"; then
            echo "    ✓ Blocked by wazero allowed_hosts (not application code)."
        elif [ "$STEP_STATUS" = "denied" ] || echo "$STEP_ERR_LC" | grep -q "no rule permits"; then
            echo "    Denied by OPA before plugin instantiation."
        fi
    fi
    fi
fi
echo ""

# ── Policy engine ──────────────────────────────────────────────────────────────
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        POLICY ENGINE (OPA / Rego)                   ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "$STATE" | jq -r '
    .plan[] | select(.agent_type == "url-fetch") |
    "    wasm-af:\(.agent_type)  →  http   →  PERMITTED"'
echo "$STATE" | jq -r '
    .plan[] | select(.agent_type == "summarizer") |
    "    wasm-af:\(.agent_type)  →  llm    →  PERMITTED"'
echo ""

# ── Dynamic allow list ─────────────────────────────────────────────────────────
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        LIVE ALLOW LIST (NATS KV, no restart)        ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""

if ! command -v nats >/dev/null 2>&1; then
    echo "  nats CLI not found — skipping live allow list demo."
    echo "  Install: https://github.com/nats-io/natscli"
    echo ""
else
    # Read and display the actual current allow list from NATS KV.
    CURRENT_LIST=$(nats kv get wasm-af-config allowed-fetch-domains 2>/dev/null \
        | grep -v '^$' | tail -1 | xargs)
    echo "  Current allow list (live from NATS KV): $CURRENT_LIST"
    echo ""

    # If example.com is already in the list (e.g., from a previous run), reset it
    # so the deny demo works correctly.
    if echo "$CURRENT_LIST" | grep -q "example.com"; then
        echo "  example.com is already in the list (leftover from a previous run)."
        echo "  Resetting to the original list..."
        nats kv put wasm-af-config allowed-fetch-domains \
            "webassembly.org,wasmcloud.com,bytecodealliance.org" > /dev/null 2>&1
        sleep 1
        echo ""
    fi

    echo "  Submitting url-fetch → https://example.com/ (not in list)..."
    DENY_ID=$(curl -sf -X POST http://localhost:8080/tasks \
        -H "Content-Type: application/json" \
        -d "$(jq -n '{type:"fan-out-summarizer", query:"allow list demo", context:{urls:"https://example.com/"}}')" \
        | jq -r '.task_id')

    if [ -z "$DENY_ID" ] || [ "$DENY_ID" = "null" ]; then
        echo "  ERROR: task submission failed"
    else
        for _ in $(seq 1 15); do
            DENY_STATE=$(curl -sf "http://localhost:8080/tasks/${DENY_ID}" || echo '{}')
            DENY_STATUS=$(echo "$DENY_STATE" | jq -r '.status // "unknown"')
            [ "$DENY_STATUS" = "completed" ] || [ "$DENY_STATUS" = "failed" ] && break
            sleep 2
        done
        STEP_STATUS=$(echo "$DENY_STATE" | jq -r '.plan[0].status // "unknown"')
        STEP_ERR=$(echo "$DENY_STATE" | jq -r '.plan[0].error // ""')
        echo "    url-fetch step: $STEP_STATUS"
        [ -n "$STEP_ERR" ] && echo "    reason: $STEP_ERR"
        [ "$STEP_STATUS" = "denied" ] && echo "    ✓ Denied — no WASM loaded."
    fi
    echo ""

    echo "  Adding example.com to the live allow list (no restart):"
    echo "    nats kv put wasm-af-config allowed-fetch-domains \\"
    echo "      'webassembly.org,wasmcloud.com,bytecodealliance.org,example.com'"
    nats kv put wasm-af-config allowed-fetch-domains \
        "webassembly.org,wasmcloud.com,bytecodealliance.org,example.com" > /dev/null 2>&1
    sleep 1  # KV watcher propagation (typically <100ms; 1s is conservative)
    echo ""

    echo "  Submitting url-fetch → https://example.com/ (now in list)..."
    ALLOW_ID=$(curl -sf -X POST http://localhost:8080/tasks \
        -H "Content-Type: application/json" \
        -d "$(jq -n '{type:"fan-out-summarizer", query:"allow list demo", context:{urls:"https://example.com/"}}')" \
        | jq -r '.task_id')

    if [ -z "$ALLOW_ID" ] || [ "$ALLOW_ID" = "null" ]; then
        echo "  ERROR: task submission failed"
    else
        for _ in $(seq 1 15); do
            ALLOW_STATE=$(curl -sf "http://localhost:8080/tasks/${ALLOW_ID}" || echo '{}')
            ALLOW_STATUS=$(echo "$ALLOW_STATE" | jq -r '.status // "unknown"')
            [ "$ALLOW_STATUS" = "completed" ] || [ "$ALLOW_STATUS" = "failed" ] && break
            sleep 2
        done
        STEP_STATUS=$(echo "$ALLOW_STATE" | jq -r '.plan[0].status // "unknown"')
        echo "    url-fetch step: $STEP_STATUS"
        echo ""
        echo "  Same URL. Same orchestrator process. Different allow list in NATS KV."
    fi
fi
# ── Policy-driven config injection ────────────────────────────────────────────
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        POLICY-DRIVEN CONFIG INJECTION                ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  Rego injects config into plugin manifests. No Brave key in data.json,"
echo "  so OPA injects mock_results=true into the web-search plugin."
echo ""

RESEARCH_ID=$(curl -sf -X POST http://localhost:8080/tasks \
    -H "Content-Type: application/json" \
    -d "$(jq -n \
        --arg type "research" \
        --arg query "What is WebAssembly?" \
        '{type: $type, query: $query}')" \
    | jq -r '.task_id')

if [ -z "$RESEARCH_ID" ] || [ "$RESEARCH_ID" = "null" ]; then
    echo "  ERROR: research task submission failed"
else
    echo "  Task ID: $RESEARCH_ID"
    printf "  Waiting..."
    R_STATUS="unknown"
    R_STATE=""
    for _ in $(seq 1 30); do
        R_STATE=$(curl -sf "http://localhost:8080/tasks/${RESEARCH_ID}" || echo '{}')
        R_STATUS=$(echo "$R_STATE" | jq -r '.status // "unknown"')
        [ "$R_STATUS" = "completed" ] || [ "$R_STATUS" = "failed" ] && break
        printf "."
        sleep 2
    done
    echo ""

    if [ "$R_STATUS" = "completed" ]; then
        echo "    ✓ Completed. Add brave_api_key to data.json for real search results."
    else
        echo "  Research task status: $R_STATUS"
        [ "$R_STATUS" = "failed" ] && echo "  Error: $(echo "$R_STATE" | jq -r '.error')"
    fi
fi
echo ""
echo "  Done."
