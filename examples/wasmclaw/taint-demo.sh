#!/usr/bin/env bash
# taint-demo — Taint Tracking Demo
#
# Demonstrates three taint-tracking behaviors using real web search (Brave API)
# and real LLM inference:
#
#   1. Taint blocks shell:   web-search → shell (DENIED)
#   2. Taint triggers gate:  web-search → responder (APPROVAL REQUIRED)
#   3. Declassification:     web-search → summarizer → responder (runs clean)
#
# Usage:
#   make taint-demo                     (needs NV_API_KEY + BRAVE_API_KEY in .env)
#   LLM_MODE=api ./taint-demo.sh
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Load .env so BRAVE_API_KEY is available before we generate the OPA data file.
[ -f "$ROOT/.env" ] && set -a && . "$ROOT/.env" && set +a

BRAVE_API_KEY="${BRAVE_API_KEY:-}"
if [ -z "$BRAVE_API_KEY" ]; then
    echo "ERROR: BRAVE_API_KEY is required. Add it to .env or export it." >&2
    exit 1
fi

# Generate a data.json that includes the Brave API key in secrets.
_TAINT_DATA="/tmp/wasmclaw-taint-data.json"
jq --arg k "$BRAVE_API_KEY" '.secrets.brave_api_key = $k' \
    "$SCRIPT_DIR/data.json" > "$_TAINT_DATA"
export OPA_DATA="$_TAINT_DATA"

# Now source the shared setup (builds, starts NATS + orchestrator).
source "$SCRIPT_DIR/lib/setup.sh"

API="http://localhost:8080"
POLL_MAX=180

echo ""
box "TAINT TRACKING DEMO"
echo ""
echo "  Taint tracking labels data with its provenance and propagates"
echo "  labels through the DAG. OPA policy gates actions based on whether"
echo "  tainted data is present in a step's context."
echo ""
echo "  Taint source:   ${BLD}web-search${RST}  →  output_taint: [\"web\"]"
echo "  Declassifier:   ${BLD}summarizer${RST}  →  declassifies: [\"web\"]"
echo ""

# ── Helpers ──────────────────────────────────────────────────────────────────

submit_generic() {
    local steps_json="$1"
    local message="$2"
    curl -sf -X POST "$API/tasks" \
        -H "Content-Type: application/json" \
        -d "$(jq -n \
            --arg m "$message" \
            --arg s "$steps_json" \
            '{type:"taint-demo", query:$m, context:{message:$m, steps:$s}}')" \
        | jq -r '.task_id // ""'
}

# Poll a task until it reaches a terminal or approval state.
# Sets _POLL_STATUS and _POLL_STATE as side effects (avoids stdout capture issues).
_POLL_STATUS=""
_POLL_STATE=""
poll_task() {
    local tid="$1" max="${2:-$POLL_MAX}"
    _POLL_STATUS="" _POLL_STATE=""
    local i=0
    while [ $i -lt "$max" ]; do
        _POLL_STATE=$(curl -sf "$API/tasks/$tid" || echo '{}')
        _POLL_STATUS=$(echo "$_POLL_STATE" | jq -r '.status // "unknown"')
        case "$_POLL_STATUS" in
            completed|failed|awaiting_approval) break ;;
        esac
        printf "·"
        sleep 1
        i=$((i + 1))
    done
}

show_lifecycle() {
    local tid="$1"
    [ -z "$tid" ] && return
    echo "    ${DIM}Lifecycle:${RST}"
    _lifecycle_lines "$tid"
    echo ""
}

show_taint() {
    local tid="$1"
    local taint
    taint=$(curl -sf "$API/tasks/$tid" | jq -r '.taint // {} | to_entries[] | "      \(.key): [\(.value | join(", "))]"' 2>/dev/null)
    if [ -n "$taint" ]; then
        echo "    ${DIM}Taint map:${RST}"
        echo "$taint"
    else
        echo "    ${DIM}Taint map: (empty)${RST}"
    fi
    echo ""
}

# ══════════════════════════════════════════════════════════════════════════════
box "1. TAINT BLOCKS SHELL"
echo ""
echo "  DAG: web-search  →  shell"
echo "  web-search output is tainted [web]."
echo "  Shell is ${RED}DENIED${RST}: policy blocks tainted data from reaching exec."
echo ""

STEPS1='[{"agent_type":"web-search","params":{"query":"WebAssembly security"}},{"agent_type":"shell","depends_on":["0"],"params":{"command":"echo results"}}]'

echo "  → ${BLD}Submitting: web-search → shell${RST}"
TID1=$(submit_generic "$STEPS1" "search then echo")
if [ -z "$TID1" ] || [ "$TID1" = "null" ]; then
    echo "    ${RED}(submit failed)${RST}"
else
    echo "    Task: ${DIM}$TID1${RST}"
    printf "    ${DIM}⏱  Running (API) "
    poll_task "$TID1"
    printf " ${RST}\n"

    if [ "$_POLL_STATUS" = "failed" ]; then
        ERR=$(echo "$_POLL_STATE" | jq -r '.error // "unknown"')
        echo "    ${RED}✗ Task failed:${RST} $ERR"
        echo "    ${GRN}^ This is expected — shell step was denied by taint policy.${RST}"
    else
        echo "    Status: $_POLL_STATUS"
    fi
    echo ""
    show_taint "$TID1"
    show_lifecycle "$TID1"
fi

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

# ══════════════════════════════════════════════════════════════════════════════
box "2. TAINT TRIGGERS LLM APPROVAL GATE"
echo ""
echo "  DAG: web-search  →  responder"
echo "  web-search output is tainted [web]."
echo "  Responder uses llm_complete → ${YLW}APPROVAL REQUIRED${RST}"
echo "  (taint_gates_enabled + web taint + LLM = human must approve)."
echo ""

STEPS2='[{"agent_type":"web-search","params":{"query":"latest Rust programming news"}},{"agent_type":"responder","depends_on":["0"]}]'

echo "  → ${BLD}Submitting: web-search → responder${RST}"
TID2=$(submit_generic "$STEPS2" "summarize what you found about Rust")
if [ -z "$TID2" ] || [ "$TID2" = "null" ]; then
    echo "    ${RED}(submit failed)${RST}"
else
    echo "    Task: ${DIM}$TID2${RST}"
    printf "    ${DIM}⏱  Running (API) "
    poll_task "$TID2"
    printf " ${RST}\n"

    if [ "$_POLL_STATUS" = "awaiting_approval" ]; then
        APPR_STEP=$(echo "$_POLL_STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].id // ""')
        APPR_AGENT=$(echo "$_POLL_STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].agent_type // ""')
        APPR_REASON=$(echo "$_POLL_STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].approval_reason // ""')

        echo ""
        echo "    ${YLW}⏸  Step paused: ${BLD}${APPR_AGENT}${RST}${YLW} (${APPR_STEP})${RST}"
        echo "    ${YLW}   Reason: ${APPR_REASON}${RST}"
        echo "    ${GRN}^ Taint gate fired — web data cannot reach LLM without approval.${RST}"
        echo ""

        show_taint "$TID2"

        printf "    ${BLD}Approve? [y/N]:${RST} "
        read -r APPR_CHOICE </dev/tty

        if [ "$APPR_CHOICE" = "y" ] || [ "$APPR_CHOICE" = "Y" ]; then
            echo ""
            echo "    ${GRN}Approving...${RST}"
            curl -sf -X POST "$API/tasks/$TID2/steps/$APPR_STEP/approve" \
                -H "Content-Type: application/json" \
                -d '{"approved_by":"demo-operator"}' > /dev/null

            printf "    ${DIM}⏱  Resuming "
            j=0
            while [ $j -lt 120 ]; do
                _POLL_STATE=$(curl -sf "$API/tasks/$TID2" || echo '{}')
                S2=$(echo "$_POLL_STATE" | jq -r '.status // "unknown"')
                [ "$S2" = "completed" ] || [ "$S2" = "failed" ] && break
                printf "·"
                sleep 1
                j=$((j + 1))
            done
            printf " ${RST}\n"

            if [ "$S2" = "completed" ]; then
                RESP=$(echo "$_POLL_STATE" | jq -r '.plan[] | select(.agent_type == "responder") | .output_key' 2>/dev/null)
                if [ -n "$RESP" ] && [ "$RESP" != "null" ]; then
                    RESP_TEXT=$(echo "$_POLL_STATE" | jq -r --arg k "$RESP" '.results[$k] // "{}"' | jq -r '.response // empty' 2>/dev/null)
                    [ -n "$RESP_TEXT" ] && echo "    ${GRN}▹${RST} $(echo "$RESP_TEXT" | head -c 400)"
                fi
            elif [ "$S2" = "failed" ]; then
                echo "    ${RED}✗ Failed after approval${RST}"
            fi
        else
            echo ""
            echo "    ${RED}Rejecting...${RST}"
            curl -sf -X POST "$API/tasks/$TID2/steps/$APPR_STEP/reject" \
                -H "Content-Type: application/json" \
                -d '{"rejected_by":"demo-operator","reason":"operator declined — tainted data not trusted"}' > /dev/null
            sleep 1
            echo "    ${RED}✗ Step rejected — LLM not invoked with tainted data.${RST}"
        fi
    elif [ "$_POLL_STATUS" = "completed" ]; then
        echo "    ${GRN}✓ Completed${RST} (approval gate may not have fired)"
    elif [ "$_POLL_STATUS" = "failed" ]; then
        ERR=$(echo "$_POLL_STATE" | jq -r '.error // "unknown"')
        echo "    ${RED}✗ Failed:${RST} $ERR"
    fi
    echo ""
    show_lifecycle "$TID2"
fi

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

# ══════════════════════════════════════════════════════════════════════════════
box "3. DECLASSIFICATION LIFTS THE GATE"
echo ""
echo "  DAG: web-search  →  summarizer  →  responder"
echo "  web-search output is tainted [web]."
echo "  Summarizer has declassifies: [\"web\"] — strips the taint label."
echo "  Responder runs ${GRN}WITHOUT approval${RST} because context_taint is now empty."
echo ""

STEPS3='[{"agent_type":"web-search","params":{"query":"WebAssembly component model"}},{"agent_type":"summarizer","depends_on":["0"],"params":{"query":"WebAssembly component model"}},{"agent_type":"responder","depends_on":["1"]}]'

echo "  → ${BLD}Submitting: web-search → summarizer → responder${RST}"
TID3=$(submit_generic "$STEPS3" "explain the WebAssembly component model")
if [ -z "$TID3" ] || [ "$TID3" = "null" ]; then
    echo "    ${RED}(submit failed)${RST}"
else
    echo "    Task: ${DIM}$TID3${RST}"
    printf "    ${DIM}⏱  Running (API) "
    poll_task "$TID3"
    printf " ${RST}\n"

    if [ "$_POLL_STATUS" = "completed" ]; then
        RESP_KEY=$(echo "$_POLL_STATE" | jq -r '.plan[] | select(.agent_type == "responder") | .output_key' 2>/dev/null)
        if [ -n "$RESP_KEY" ] && [ "$RESP_KEY" != "null" ]; then
            RESP_TEXT=$(echo "$_POLL_STATE" | jq -r --arg k "$RESP_KEY" '.results[$k] // "{}"' | jq -r '.response // empty' 2>/dev/null)
            [ -n "$RESP_TEXT" ] && echo "    ${GRN}▹${RST} $(echo "$RESP_TEXT" | head -c 400)"
        fi
        echo ""
        echo "    ${GRN}^ No approval gate — summarizer declassified the web taint.${RST}"
    elif [ "$_POLL_STATUS" = "awaiting_approval" ]; then
        echo "    ${YLW}⏸  Approval gate fired (unexpected — declassification may not have worked)${RST}"
    elif [ "$_POLL_STATUS" = "failed" ]; then
        ERR=$(echo "$_POLL_STATE" | jq -r '.error // "unknown"')
        echo "    ${RED}✗ Failed:${RST} $ERR"
    fi
    echo ""
    show_taint "$TID3"
    show_lifecycle "$TID3"
fi

# ══════════════════════════════════════════════════════════════════════════════
echo ""
echo "  ${BGRN}Done.${RST}"
