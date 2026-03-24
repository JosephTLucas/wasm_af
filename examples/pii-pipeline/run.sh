#!/usr/bin/env bash
# PII Pipeline — BYOA demo
#
# Demonstrates writing a Python agent, compiling it to a WASM component,
# uploading it to a running orchestrator, and executing it in a multi-agent
# DAG alongside platform agents.
#
# Pipeline:  url-fetch (HTTP) → pii-redactor (BYOA/Python) → responder (LLM)
#
# Prerequisites: rust (with wasm32-wasip2 target), jq, python3,
#                componentize-py (pip install componentize-py)
#
# Usage:
#   ./examples/pii-pipeline/run.sh
#   LLM_MODE=api ./examples/pii-pipeline/run.sh
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

[ -f "$HOME/.cargo/env" ] && . "$HOME/.cargo/env"
[ -f "$ROOT/.env" ] && set -a && . "$ROOT/.env" && set +a

LLM_MODE="${LLM_MODE:-mock}"
LLM_API_KEY="${LLM_API_KEY:-}"
LLM_MODEL="${LLM_MODEL:-gpt-4o-mini}"
DOC_PORT="${DOC_PORT:-8888}"
EXAMPLE_DIR="$ROOT/examples/pii-pipeline"

# ── ANSI colors ──────────────────────────────────────────────────────────────
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    RST=$'\033[0m'  BLD=$'\033[1m'  DIM=$'\033[2m'
    RED=$'\033[31m'  GRN=$'\033[32m'  YLW=$'\033[33m'  CYN=$'\033[36m'
    BRED=$'\033[1;31m'  BGRN=$'\033[1;32m'  BCYN=$'\033[1;36m'
else
    RST=""  BLD=""  DIM=""
    RED=""  GRN=""  YLW=""  CYN=""
    BRED=""  BGRN=""  BCYN=""
fi

box() {
    echo "  ${BCYN}╔══════════════════════════════════════════════════════╗${RST}"
    printf "  ${BCYN}║${RST}  ${BLD}%-52s${RST}${BCYN}║${RST}\n" "$1"
    echo "  ${BCYN}╚══════════════════════════════════════════════════════╝${RST}"
}

die() { echo "  ${BRED}ERROR:${RST} $1" >&2; exit 1; }

ORCH_PID=""
NATS_PID=""
NATS_STARTED_BY_US=""
NATS_STORE_DIR=""
DOC_SERVER_PID=""

cleanup() {
    [ -n "$DOC_SERVER_PID" ] && kill "$DOC_SERVER_PID" 2>/dev/null || true
    [ -n "$ORCH_PID" ] && kill "$ORCH_PID" 2>/dev/null || true
    if [ -n "$NATS_STARTED_BY_US" ] && [ -n "$NATS_PID" ]; then
        kill "$NATS_PID" 2>/dev/null || true
        [ -n "$NATS_STORE_DIR" ] && rm -rf "$NATS_STORE_DIR" 2>/dev/null || true
    fi
}
trap cleanup EXIT

echo ""
echo "  ${BCYN}╔══════════════════════════════════════════════════════╗${RST}"
echo "  ${BCYN}║${RST}  ${BLD}WASM_AF — PII Pipeline: Bring Your Own Agent Demo${RST}   ${BCYN}║${RST}"
echo "  ${BCYN}╚══════════════════════════════════════════════════════╝${RST}"
echo ""
echo "  Pipeline:  ${CYN}url-fetch${RST} → ${YLW}pii-redactor (BYOA/Python)${RST} → ${CYN}responder${RST}"
echo "  LLM mode:  ${CYN}${LLM_MODE}${RST}"
echo ""

# ══════════════════════════════════════════════════════════════════════════════
# PHASE 1: Build platform agents + orchestrator
# ══════════════════════════════════════════════════════════════════════════════
echo "  ${BLD}[1/6]${RST} Building platform components..."

if ! rustup target list --installed 2>/dev/null | grep -q wasm32-wasip2; then
    echo "        Adding wasm32-wasip2 target..."
    rustup target add wasm32-wasip2 || die "Failed to add wasm32-wasip2 target."
fi

(cd components && cargo build --release 2>&1) || die "Component build failed."
mkdir -p ./bin
cargo build --release -p wasm-af-orchestrator 2>&1 || die "Orchestrator build failed."
cp target/release/orchestrator ./bin/orchestrator
echo "        ${GRN}Done.${RST}"
echo ""

# ══════════════════════════════════════════════════════════════════════════════
# PHASE 2: Build the Python BYOA agent
# ══════════════════════════════════════════════════════════════════════════════
box "BYOA: Build the Python agent"
echo ""
echo "  ${BLD}[2/6]${RST} Building PII Redactor with componentize-py..."
echo ""
echo "  ${DIM}The agent is a Python module that implements the agent-untrusted"
echo "  WIT world. componentize-py compiles it to a WASM component.${RST}"
echo ""

bash "$EXAMPLE_DIR/agent/build.sh" 2>&1 | sed 's/^/    /'

PII_WASM="$EXAMPLE_DIR/agent/pii_redactor.wasm"
[ -f "$PII_WASM" ] || die "BYOA agent build failed: $PII_WASM not found."
echo "        ${GRN}Agent built: $(ls -lh "$PII_WASM" | awk '{print $5}')${RST}"
echo ""

# ══════════════════════════════════════════════════════════════════════════════
# PHASE 3: Start NATS
# ══════════════════════════════════════════════════════════════════════════════
echo "  ${BLD}[3/6]${RST} Starting NATS..."

if lsof -i :4222 -sTCP:LISTEN >/dev/null 2>&1; then
    echo "        NATS already running on :4222"
else
    NATS_BIN=""
    if command -v nats-server >/dev/null 2>&1; then
        NATS_BIN="nats-server"
    elif [ -x "$HOME/.local/share/wash/downloads/nats-server" ]; then
        NATS_BIN="$HOME/.local/share/wash/downloads/nats-server"
    else
        die "nats-server not found. Install via: brew install nats-server"
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
    echo "        ${GRN}NATS started${RST} ${DIM}(PID $NATS_PID)${RST}"
fi
echo ""

# ══════════════════════════════════════════════════════════════════════════════
# PHASE 4: Start orchestrator + document server
# ══════════════════════════════════════════════════════════════════════════════
echo "  ${BLD}[4/6]${RST} Starting services..."

if lsof -ti:8080 >/dev/null 2>&1; then
    echo "        Stopping stale process on :8080..."
    lsof -ti:8080 | xargs kill 2>/dev/null || true
    sleep 1
fi

if [ "$LLM_MODE" = "api" ]; then
    _LLM_BASE_URL="$LLM_BASE_URL"
    _LLM_API_KEY="$LLM_API_KEY"
    _LLM_MODEL="$LLM_MODEL"
    _PLUGIN_TIMEOUT=120
    [ -n "$LLM_API_KEY" ] || die "LLM_MODE=api requires LLM_API_KEY."
    [ -n "${LLM_BASE_URL:-}" ] || die "LLM_MODE=api requires LLM_BASE_URL."
else
    _LLM_BASE_URL="${LLM_BASE_URL:-http://localhost:11434}"
    _LLM_API_KEY="${LLM_API_KEY:-}"
    _LLM_MODEL="${MODEL:-qwen3:4b}"
    _PLUGIN_TIMEOUT=30
fi

OPA_POLICY="$EXAMPLE_DIR" \
OPA_DATA="$EXAMPLE_DIR/data.json" \
AGENT_REGISTRY_FILE="$EXAMPLE_DIR/agents.json" \
LLM_MODE="$LLM_MODE" \
LLM_BASE_URL="$_LLM_BASE_URL" \
LLM_API_KEY="$_LLM_API_KEY" \
LLM_MODEL="$_LLM_MODEL" \
PLUGIN_TIMEOUT_SEC="$_PLUGIN_TIMEOUT" \
WASM_DIR="$ROOT/components/target/wasm32-wasip2/release" \
    ./bin/orchestrator > /tmp/wasm-af-orchestrator.log 2>&1 &
ORCH_PID=$!
sleep 2

if ! curl -sf http://localhost:8080/healthz > /dev/null 2>&1; then
    cat /tmp/wasm-af-orchestrator.log
    die "Orchestrator failed to start."
fi
echo "        ${GRN}Orchestrator running${RST} ${DIM}(PID $ORCH_PID)${RST}"

# Serve the sample document on a local HTTP server.
if lsof -ti:$DOC_PORT >/dev/null 2>&1; then
    lsof -ti:$DOC_PORT | xargs kill 2>/dev/null || true
    sleep 1
fi
python3 -m http.server "$DOC_PORT" --directory "$EXAMPLE_DIR" > /dev/null 2>&1 &
DOC_SERVER_PID=$!
sleep 1
echo "        ${GRN}Document server running${RST} ${DIM}(port $DOC_PORT)${RST}"
echo ""

# ══════════════════════════════════════════════════════════════════════════════
# PHASE 5: Upload the BYOA agent
# ══════════════════════════════════════════════════════════════════════════════
box "BYOA: Upload the Python agent"
echo ""
echo "  ${BLD}[5/6]${RST} Uploading PII Redactor to the orchestrator..."
echo ""
echo "  ${DIM}curl -X POST localhost:8080/agents \\"
echo "    -F 'meta={\"name\":\"pii-redactor\",\"context_key\":\"pii_result\"}' \\"
echo "    -F 'wasm=@pii_redactor.wasm'${RST}"
echo ""

UPLOAD_RESP=$(curl -s -w "\n%{http_code}" -X POST http://localhost:8080/agents \
    -F "meta={\"name\":\"pii-redactor\",\"context_key\":\"pii_result\"}" \
    -F "wasm=@${PII_WASM}")
UPLOAD_HTTP=$(echo "$UPLOAD_RESP" | tail -1)
UPLOAD_MSG=$(echo "$UPLOAD_RESP" | head -1)

if [ "$UPLOAD_HTTP" = "201" ]; then
    echo "        ${GRN}Registered!${RST} HTTP $UPLOAD_HTTP"
else
    echo "        ${RED}Upload failed:${RST} HTTP $UPLOAD_HTTP — $UPLOAD_MSG"
    die "BYOA upload failed."
fi

echo ""
echo "  Agent registry:"
curl -sf http://localhost:8080/agents | jq -r '
    to_entries[] |
    if .value.external then
        "    \(.key)  \u001b[33m(BYOA, untrusted)\u001b[0m  context_key=\(.value.context_key)"
    else
        "    \(.key)  \u001b[36m(platform, \(.value.capability))\u001b[0m  context_key=\(.value.context_key)"
    end'
echo ""

# ══════════════════════════════════════════════════════════════════════════════
# PHASE 6: Run the pipeline
# ══════════════════════════════════════════════════════════════════════════════
box "Execute: url-fetch → pii-redactor → responder"
echo ""
echo "  ${BLD}[6/6]${RST} Submitting the PII pipeline task..."
echo ""

DOC_URL="http://localhost:${DOC_PORT}/sample-doc.html"

STEPS_JSON=$(jq -nc \
    --arg url "$DOC_URL" \
    '[
        {"agent_type":"url-fetch","params":{"url":$url}},
        {"agent_type":"pii-redactor","depends_on":["0"]},
        {"agent_type":"responder","depends_on":["1"]}
    ]')

TASK_ID=$(curl -sf -X POST http://localhost:8080/tasks \
    -H "Content-Type: application/json" \
    -d "$(jq -n \
        --arg steps "$STEPS_JSON" \
        '{type: "pii-pipeline", query: "Scan document for PII", context: {steps: $steps, message: "Summarize the PII scan findings. List each type of PII found, how many instances, and confirm they were redacted."}}')" \
    | jq -r '.task_id')

[ -z "$TASK_ID" ] || [ "$TASK_ID" = "null" ] && die "Failed to submit task."
echo "        Task ID: ${CYN}$TASK_ID${RST}"
echo ""

# ── Poll for the approval gate ────────────────────────────────────────────────
echo "  Waiting for pipeline to reach the approval gate..."
for _ in $(seq 1 20); do
    STATE=$(curl -sf "http://localhost:8080/tasks/${TASK_ID}" || echo '{}')
    # Check if the BYOA step is awaiting approval
    BYOA_STATUS=$(echo "$STATE" | jq -r '.plan[] | select(.agent_type == "pii-redactor") | .status' 2>/dev/null)
    [ "$BYOA_STATUS" = "awaiting_approval" ] && break
    TASK_STATUS=$(echo "$STATE" | jq -r '.status // "unknown"')
    [ "$TASK_STATUS" = "completed" ] || [ "$TASK_STATUS" = "failed" ] && break
    sleep 1
done

if [ "$BYOA_STATUS" = "awaiting_approval" ]; then
    echo ""
    box "APPROVAL GATE"
    echo ""
    echo "  The ${YLW}pii-redactor${RST} agent is ${YLW}awaiting human approval${RST}."
    echo "  This is the untrusted-agent sandbox in action: OPA policy requires"
    echo "  approval before any BYOA agent executes."
    echo ""

    # Show the step details
    echo "  ${DIM}Pipeline state:${RST}"
    echo "$STATE" | jq -r --arg g "$GRN" --arg y "$YLW" --arg c "$CYN" --arg R "$RST" --arg d "$DIM" '
        .plan[] |
        if .status == "completed" then
            "    \($g)✓\($R) \(.agent_type)  \($d)[\(.status)]\($R)"
        elif .status == "awaiting_approval" then
            "    \($y)⏸\($R) \(.agent_type)  \($y)[\(.status)]\($R)"
        else
            "    · \(.agent_type)  \($d)[\(.status)]\($R)"
        end'
    echo ""

    # Show approval endpoint
    BYOA_STEP_ID=$(echo "$STATE" | jq -r '.plan[] | select(.agent_type == "pii-redactor") | .id')
    echo "  ${DIM}Approval endpoint:"
    echo "    POST /tasks/$TASK_ID/steps/$BYOA_STEP_ID/approve${RST}"
    echo ""

    # Auto-approve
    echo "  Approving the BYOA agent..."
    APPROVE_RESP=$(curl -sf -X POST "http://localhost:8080/tasks/${TASK_ID}/steps/${BYOA_STEP_ID}/approve" \
        -H "Content-Type: application/json" \
        -d '{"approved_by": "demo-operator"}' || echo "error")
    echo "        ${GRN}Approved.${RST}"
    echo ""
fi

# ── Wait for completion ───────────────────────────────────────────────────────
echo "  Waiting for pipeline to complete..."
printf "        "
for _ in $(seq 1 60); do
    STATE=$(curl -sf "http://localhost:8080/tasks/${TASK_ID}" || echo '{}')
    STATUS=$(echo "$STATE" | jq -r '.status // "unknown"')
    [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ] && break
    printf "."
    sleep 2
done
echo ""
echo ""

[ "$STATUS" = "failed" ] && {
    echo "  ${RED}Task FAILED:${RST}"
    echo "$STATE" | jq -r '.error // empty'
    echo "$STATE" | jq -r '.plan[] | select(.status == "failed" or .status == "denied") | "  \(.agent_type): \(.error // .status)"'
    exit 1
}
[ "$STATUS" != "completed" ] && die "Task did not complete within 120 seconds (status: $STATUS)"

# ══════════════════════════════════════════════════════════════════════════════
# RESULTS
# ══════════════════════════════════════════════════════════════════════════════
echo ""
box "RESULTS"
echo ""

# Step status
echo "  ${BLD}Pipeline execution:${RST}"
echo "$STATE" | jq -r --arg g "$GRN" --arg y "$YLW" --arg c "$CYN" --arg R "$RST" --arg d "$DIM" '
    .plan[] |
    if .status == "completed" then
        "    \($g)✓\($R) \(.agent_type)  \($d)[\(.status)]\($R)"
    elif .status == "denied" then
        "    \($c)✗\($R) \(.agent_type)  \($c)[\(.status)]\($R)"
    else
        "    · \(.agent_type)  \($d)[\(.status)]\($R)"
    end'
echo ""

# ── PII Findings ──────────────────────────────────────────────────────────────
box "PII FINDINGS (from BYOA Python agent)"
echo ""

PII_OUTPUT_KEY=$(echo "$STATE" | jq -r '.plan[] | select(.agent_type == "pii-redactor") | .output_key')
PII_RESULT=$(echo "$STATE" | jq -r ".results[\"$PII_OUTPUT_KEY\"] // empty")

if [ -n "$PII_RESULT" ]; then
    echo "  ${BLD}Summary:${RST}"
    echo "$PII_RESULT" | jq -r '
        .summary |
        "    Total PII found:  \(.total_pii_found)",
        "    Types detected:   \(.types_found | join(", "))",
        "    Original length:  \(.original_length) chars",
        "    Redacted length:  \(.redacted_length) chars"'
    echo ""

    echo "  ${BLD}Breakdown by type:${RST}"
    echo "$PII_RESULT" | jq -r '
        .findings | to_entries[] |
        "    \(.key): \(.value.count) found  (\(.value.description))"'
    echo ""

    echo "  ${BLD}Redacted text (first 500 chars):${RST}"
    echo "$PII_RESULT" | jq -r '.redacted_text[:500]' | sed 's/^/    /'
    echo "    ${DIM}...${RST}"
    echo ""
fi

# ── Capability Isolation ──────────────────────────────────────────────────────
box "CAPABILITY ISOLATION"
echo ""
echo "  ${BLD}Trust tiers in this pipeline:${RST}"
echo ""
echo "    ${CYN}url-fetch${RST}      capability: ${CYN}http${RST}       host_fns: [http]"
echo "    ${YLW}pii-redactor${RST}   capability: ${YLW}untrusted${RST}  host_fns: ${RED}[]${RST}  ${DIM}← no LLM, no HTTP, no shell${RST}"
echo "    ${CYN}responder${RST}      capability: ${CYN}llm${RST}        host_fns: [llm_complete]"
echo ""
echo "  The BYOA agent ran in a ${YLW}16 MiB sandbox${RST} with ${YLW}10s timeout${RST}."
echo "  It ${RED}cannot${RST} access the network, LLM, filesystem, or shell —"
echo "  those capabilities are ${BLD}structurally absent${RST} from its Linker."
echo ""

# ── LLM Summary (if not mock) ────────────────────────────────────────────────
RESP_OUTPUT_KEY=$(echo "$STATE" | jq -r '.plan[] | select(.agent_type == "responder") | .output_key')
RESP_RESULT=$(echo "$STATE" | jq -r ".results[\"$RESP_OUTPUT_KEY\"] // empty")

if [ -n "$RESP_RESULT" ]; then
    box "LLM SUMMARY (from platform responder)"
    echo ""
    echo "$RESP_RESULT" | jq -r '.response // empty' | fold -s -w 72 | sed 's/^/    /'
    echo ""
fi

echo "  ${GRN}Done.${RST}"
echo ""
