#!/usr/bin/env bash
# Fan-Out Summarizer — end-to-end demo
#
# Builds everything, starts NATS, runs the orchestrator, submits a task,
# and displays the results with security context.
#
# Prerequisites: rust (with wasm32-unknown-unknown target), go, jq
# Optional: nats-server OR wash CLI (for bundled NATS)
#
# Usage:
#   ./examples/fan-out-summarizer/run.sh
#   URLS="https://a.com,https://b.com" ./examples/fan-out-summarizer/run.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

URLS="${URLS:-https://webassembly.org,https://wasmcloud.com,https://bytecodealliance.org}"
QUERY="${QUERY:-Compare these WebAssembly ecosystem projects}"
ORCH_PID=""
NATS_PID=""

cleanup() {
    [ -n "$ORCH_PID" ] && kill "$ORCH_PID" 2>/dev/null || true
    [ -n "$NATS_PID" ] && kill "$NATS_PID" 2>/dev/null || true
}
trap cleanup EXIT

# ── Header ────────────────────────────────────────────────────────────────────
echo ""
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║          WASM_AF — Fan-Out Summarizer Demo          ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""

# ── 1. Build ──────────────────────────────────────────────────────────────────
echo "  [1/4] Building..."

if ! rustup target list --installed 2>/dev/null | grep -q wasm32-unknown-unknown; then
    echo "        Adding wasm32-unknown-unknown target..."
    rustup target add wasm32-unknown-unknown >/dev/null 2>&1
fi

echo "        Rust WASM plugins..."
(cd components && cargo build --release 2>&1 | tail -1)

echo "        Go orchestrator..."
go build -o ./bin/orchestrator ./provider/orchestrator/ 2>&1
echo "        Done."
echo ""

# ── 2. Start NATS ────────────────────────────────────────────────────────────
echo "  [2/4] Starting NATS..."

# Try nats-server directly, fall back to wash's bundled copy
NATS_BIN=""
if command -v nats-server >/dev/null 2>&1; then
    NATS_BIN="nats-server"
elif [ -x "$HOME/.local/share/wash/downloads/nats-server" ]; then
    NATS_BIN="$HOME/.local/share/wash/downloads/nats-server"
else
    echo "        ERROR: nats-server not found. Install NATS or run 'wash up' once to download it."
    exit 1
fi

$NATS_BIN -js -p 4222 > /dev/null 2>&1 &
NATS_PID=$!
sleep 2
echo "        NATS running (PID $NATS_PID)"
echo ""

# ── 3. Start orchestrator ────────────────────────────────────────────────────
echo "  [3/4] Starting orchestrator..."

POLICY_RULES_FILE="$ROOT/examples/fan-out-summarizer/policies.json" \
LLM_MODE=mock \
WASM_DIR="$ROOT/components/target/wasm32-unknown-unknown/release" \
    ./bin/orchestrator > /tmp/wasm-af-orchestrator.log 2>&1 &
ORCH_PID=$!
sleep 2

if ! curl -sf http://localhost:8080/healthz > /dev/null 2>&1; then
    echo "        ERROR: orchestrator failed to start. Check /tmp/wasm-af-orchestrator.log"
    exit 1
fi
echo "        Orchestrator running (PID $ORCH_PID)"
echo ""

# ── 4. Submit task and display results ────────────────────────────────────────
echo "  [4/4] Submitting task..."
echo ""
echo "        Query: $QUERY"
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

echo "        Task ID: $TASK_ID"
echo ""

# Poll until complete
echo "        Waiting for completion..."
for i in $(seq 1 30); do
    STATE=$(curl -sf "http://localhost:8080/tasks/${TASK_ID}")
    STATUS=$(echo "$STATE" | jq -r '.status')

    if [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ]; then
        break
    fi
    sleep 2
done
echo ""

if [ "$STATUS" = "failed" ]; then
    echo "  Task FAILED:"
    echo "$STATE" | jq -r '.error'
    exit 1
fi

# ── Display results ──────────────────────────────────────────────────────────
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║                     RESULTS                         ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""

echo "  Security Model:"
echo "  ───────────────"
echo "$STATE" | jq -r '.plan[] | select(.agent_type == "url-fetch") |
    "    \(.agent_type) [\(.id | split("-") | last)]  allowed_hosts: \(.allowed_hosts)  status: \(.status)"'
echo "$STATE" | jq -r '.plan[] | select(.agent_type == "summarizer") |
    "    \(.agent_type)          allowed_hosts: (none — LLM only)   status: \(.status)"'
echo ""
echo "    Policy: url-fetch may use HTTP (scoped per-instance)"
echo "            summarizer may use LLM (no HTTP access)"
echo "            All other capability requests: DENIED"
echo ""

echo "  Fetched Pages:"
echo "  ──────────────"
echo "$STATE" | jq -r '
    .plan[] | select(.agent_type == "url-fetch") |
    .id as $id |
    (.id + ".output") as $key |
    $key' | while read -r key; do
    RESULT=$(echo "$STATE" | jq -r --arg k "$key" '.results[$k]')
    if [ "$RESULT" != "null" ]; then
        TITLE=$(echo "$RESULT" | jq -r '.results[0].title // "unknown"')
        URL=$(echo "$RESULT" | jq -r '.query')
        SNIPPET_LEN=$(echo "$RESULT" | jq -r '.results[0].snippet | length')
        echo "    $TITLE"
        echo "      URL: $URL"
        echo "      Content: ${SNIPPET_LEN} chars fetched"
        echo ""
    fi
done

echo "  Summary (mock LLM):"
echo "  ────────────────────"
SUM_KEY=$(echo "$STATE" | jq -r '.plan[] | select(.agent_type == "summarizer") | .id + ".output"')
echo "$STATE" | jq -r --arg k "$SUM_KEY" '.results[$k]' | jq -r '.summary' | head -5 | sed 's/^/    /'
echo "    ..."
echo ""

echo "  Architecture:"
echo "  ─────────────"
echo "    3 url-fetch WASM plugins ran in parallel (separate Extism instances)"
echo "    Each was created -> invoked -> destroyed within its step"
echo "    Each had a different allowed_hosts — enforced by the WASM runtime"
echo "    Results merged by orchestrator, passed to summarizer as context"
echo "    Summarizer called mock LLM via host function (url-fetch cannot)"
echo ""
echo "  Done."
