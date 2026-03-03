#!/usr/bin/env bash
# wasmclaw — Personal AI Assistant Demo
#
# Usage:
#   make demo                        (recommended — mock LLM)
#   LLM_MODE=api make demo           (NVIDIA NIM API — needs NV_API_KEY)
#   LLM_MODE=real make demo          (local Ollama)
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# If BRAVE_API_KEY is available, inject it into OPA data so the taint tracking
# scenarios use real Brave Search. Otherwise web-search falls back to mock results.
[ -f "$ROOT/.env" ] && set -a && . "$ROOT/.env" && set +a
if [ -n "${BRAVE_API_KEY:-}" ]; then
    _RUN_DATA="/tmp/wasmclaw-run-data.json"
    jq --arg k "$BRAVE_API_KEY" '.secrets.brave_api_key = $k' \
        "$SCRIPT_DIR/data.json" > "$_RUN_DATA"
    export OPA_DATA="$_RUN_DATA"
fi

source "$SCRIPT_DIR/lib/setup.sh"

echo ""
box "WASM_AF — wasmclaw Demo"
echo ""

# ── OPA unit tests ────────────────────────────────────────────────────────────
echo "  ${BLD}OPA policy tests...${RST}"

if command -v opa >/dev/null 2>&1; then
    OPA_OUTPUT=$(opa test "$EXAMPLE_DIR" 2>&1)
    OPA_EXIT=$?
    OPA_SUMMARY=$(echo "$OPA_OUTPUT" | tail -1)
    if [ "$OPA_EXIT" -eq 0 ]; then
        echo "        ${BGRN}$OPA_SUMMARY${RST}"
    else
        echo "$OPA_OUTPUT" | sed 's/^/       /'
        echo "        ${BRED}Some policy tests FAILED.${RST}"
    fi
else
    echo "        ${YLW}(opa CLI not found — skipped)${RST}"
fi
echo ""

# ── Runtime demo ──────────────────────────────────────────────────────────────
echo "  ${BLD}Runtime demo${RST} ${DIM}(inference: $LLM_LABEL)${RST}"
echo ""

# ── Helpers ───────────────────────────────────────────────────────────────────

_lifecycle_lines() {
    local tid="$1"
    grep "$tid" /tmp/wasm-af-orchestrator.log 2>/dev/null \
        | jq -r --arg g "$GRN" --arg r "$RED" --arg y "$YLW" --arg d "$DIM" --arg R "$RST" --arg b "$BLD" \
          '(.fields.message // .msg) as $m | (.timestamp // .time) as $t |
          select($m == "starting step" or $m == "plugin created" or $m == "plugin destroyed" or $m == "step completed" or $m == "step denied" or $m == "step awaiting approval" or $m == "step approved" or $m == "task parked, awaiting approval" or $m == "task finished") |
          (.fields.agent_type // .fields.agent // .agent_type // "") as $at |
          if $m == "starting step" then
            "      \($d)\($t[11:23])\($R)  \($b)▶\($R) \($at)"
          elif $m == "plugin created" then
            "      \($d)\($t[11:23])    ↳ wasm created  (host_fns: \(.fields.host_fns // .host_fns // 0), instantiate: \(.fields.create_ms // .create_ms // 0)ms)\($R)"
          elif $m == "plugin destroyed" then
            "      \($d)\($t[11:23])    ↳ wasm destroyed (exec: \(.fields.exec_ms // .exec_ms // 0)ms)\($R)"
          elif $m == "step denied" then
            "      \($d)\($t[11:23])\($R)  \($r)✗ \($at) denied\($R)"
          elif $m == "step completed" then
            "      \($d)\($t[11:23])\($R)  \($g)✓ \($at)\($R)"
          elif $m == "step awaiting approval" then
            "      \($d)\($t[11:23])\($R)  \($y)⏸ \($at) awaiting approval\($R) \($d)(\(.fields.reason // .reason // ""))\($R)"
          elif $m == "step approved" then
            "      \($d)\($t[11:23])\($R)  \($g)✓ approved\($R) \($d)(by \(.fields.approved_by // .approved_by // "unknown"))\($R)"
          elif $m == "task parked, awaiting approval" then
            "      \($d)\($t[11:23])    ↳ task parked — waiting for human decision\($R)"
          elif $m == "task finished" then
            "      \($d)\($t[11:23])    ↳ task \(.fields.status // "done")\($R)"
          else empty end' 2>/dev/null || true
}

show_lifecycle() {
    local tid="$1"
    [ -z "$tid" ] && return
    echo "    ${DIM}Lifecycle:${RST}"
    _lifecycle_lines "$tid"
    echo ""
}

DEFAULT_POLL_TIMEOUT=90
[ "$LLM_MODE" = "api" ] && DEFAULT_POLL_TIMEOUT=180

# Extract the best response from a completed task state JSON.
# Prefers the responder output; falls back to the last completed step.
_extract_response() {
    local state="$1"
    local resp_key resp
    resp_key=$(echo "$state" | jq -r '.plan[] | select(.agent_type == "responder") | .output_key' 2>/dev/null)
    if [ -n "$resp_key" ] && [ "$resp_key" != "null" ]; then
        resp=$(echo "$state" | jq -r --arg k "$resp_key" '.results[$k] // "{}"' 2>/dev/null \
            | jq -r '.response // empty' 2>/dev/null)
        [ -n "$resp" ] && { echo "$resp" | head -c 400; return; }
    fi
    # Fallback: last completed step's output (for skill-demo tasks without a responder).
    local last_key last_val
    last_key=$(echo "$state" | jq -r '[.plan[] | select(.status == "completed")] | last | .output_key // ""' 2>/dev/null)
    if [ -n "$last_key" ] && [ "$last_key" != "null" ]; then
        last_val=$(echo "$state" | jq -r --arg k "$last_key" '.results[$k] // "N/A"' 2>/dev/null)
        echo "$last_val" | head -c 400
    else
        echo "N/A"
    fi
}

submit_and_poll() {
    local label="$1" msg="$2" max_poll="${3:-}" task_type="${4:-chat}"
    [ -z "$max_poll" ] && max_poll="$DEFAULT_POLL_TIMEOUT"
    echo "  → ${BLD}$label${RST}"
    echo "    ${CYN}▸ $msg${RST}"

    local BODY
    BODY=$(curl -sf -X POST http://localhost:8080/tasks \
        -H "Content-Type: application/json" \
        -d "$(jq -n --arg m "$msg" --arg t "$task_type" '{type:$t,query:$m,context:{message:$m}}')")

    local TID
    TID=$(echo "$BODY" | jq -r '.task_id // ""')
    if [ -z "$TID" ] || [ "$TID" = "null" ]; then
        echo "    ${RED}(submit failed: $BODY)${RST}"
        echo ""
        return
    fi

    local poll_start=$SECONDS
    printf "    ${DIM}⏱  Running (%s) " "$LLM_LABEL"
    local STATUS="" STATE="" i=0
    while [ $i -lt "$max_poll" ]; do
        STATE=$(curl -sf "http://localhost:8080/tasks/${TID}" || echo '{}')
        STATUS=$(echo "$STATE" | jq -r '.status // "unknown"')
        [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ] && break
        printf "·"
        sleep 1
        i=$((i + 1))
    done
    local elapsed=$(( SECONDS - poll_start ))
    printf " %ds${RST}\n" "$elapsed"

    if [ "$STATUS" = "completed" ]; then
        echo "    ${GRN}▹${RST} $(_extract_response "$STATE")"
    elif [ "$STATUS" = "failed" ]; then
        local TASK_ERR
        TASK_ERR=$(echo "$STATE" | jq -r '.error // "unknown"' 2>/dev/null)
        echo "    ${RED}✗ Failed:${RST} $TASK_ERR"
    else
        echo "    ${YLW}⚠ Timed out after ${max_poll}s (status: $STATUS)${RST}"
    fi

    show_lifecycle "$TID"
}

_LAST_TID=""
submit_and_poll_quiet() {
    local label="$1" msg="$2" max_poll="${3:-}" task_type="${4:-chat}"
    [ -z "$max_poll" ] && max_poll="$DEFAULT_POLL_TIMEOUT"
    _LAST_TID=""
    echo "  → ${BLD}$label${RST}"
    echo "    ${CYN}▸ $msg${RST}"

    local BODY
    BODY=$(curl -sf -X POST http://localhost:8080/tasks \
        -H "Content-Type: application/json" \
        -d "$(jq -n --arg m "$msg" --arg t "$task_type" '{type:$t,query:$m,context:{message:$m}}')")

    local TID
    TID=$(echo "$BODY" | jq -r '.task_id // ""')
    if [ -z "$TID" ] || [ "$TID" = "null" ]; then
        echo "    ${RED}(submit failed: $BODY)${RST}"
        return
    fi
    _LAST_TID="$TID"

    local poll_start=$SECONDS
    printf "    ${DIM}⏱  Running (%s) " "$LLM_LABEL"
    local STATUS="" STATE="" i=0
    while [ $i -lt "$max_poll" ]; do
        STATE=$(curl -sf "http://localhost:8080/tasks/${TID}" || echo '{}')
        STATUS=$(echo "$STATE" | jq -r '.status // "unknown"')
        [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ] && break
        printf "·"
        sleep 1
        i=$((i + 1))
    done
    local elapsed=$(( SECONDS - poll_start ))
    printf " %ds${RST}\n" "$elapsed"

    if [ "$STATUS" = "completed" ]; then
        echo "    ${GRN}▹${RST} $(_extract_response "$STATE")"
    elif [ "$STATUS" = "failed" ]; then
        local TASK_ERR
        TASK_ERR=$(echo "$STATE" | jq -r '.error // "unknown"' 2>/dev/null)
        echo "    ${RED}✗ Failed:${RST} $TASK_ERR"
    else
        echo "    ${YLW}⚠ Timed out after ${max_poll}s (status: $STATUS)${RST}"
    fi
}

# ══════════════════════════════════════════════════════════════════════════════
box "SKILL EXECUTION (LLM routes to agents)"
echo ""

submit_and_poll "Shell agent (std::process::Command, path-confined)" \
    "list files in /tmp/wasmclaw" "" "skill-demo"

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

submit_and_poll "Sandbox-exec agent (Python-in-WASM via wasmtime)" \
    "calculate fibonacci of 10" "" "skill-demo"

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

echo "  ${BLD}File-ops: write then read${RST} ${DIM}(WASI std::fs, wasmtime preopened_dir)${RST}"
echo ""

submit_and_poll_quiet "File-ops write" \
    "write wasmclaw sandbox test to /tmp/wasmclaw/demo.txt" "" "skill-demo"
WRITE_TID="$_LAST_TID"
echo ""

submit_and_poll_quiet "File-ops read back" \
    "read /tmp/wasmclaw/demo.txt" "" "skill-demo"
READ_TID="$_LAST_TID"

echo "    ${DIM}Lifecycle (write → destroy → read):${RST}"
_lifecycle_lines "$WRITE_TID"
echo "      ${DIM}────── instance destroyed ── file persists on host ──────${RST}"
_lifecycle_lines "$READ_TID"
echo ""

# ══════════════════════════════════════════════════════════════════════════════
box "SECURITY BOUNDARIES"
echo ""

echo "  ${BLD}1. Submission gate${RST} ${DIM}— deny before any plan is built${RST}"
echo "  → ${BLD}Submitting type='research'${RST} ${DIM}(not in allowed task types)${RST}"
DENY_BODY=$(curl -s -w "\n%{http_code}" -X POST http://localhost:8080/tasks \
    -H "Content-Type: application/json" \
    -d '{"type":"research","query":"this should be rejected"}')
DENY_HTTP=$(echo "$DENY_BODY" | tail -1)
DENY_MSG=$(echo "$DENY_BODY" | head -1)
if [ "$DENY_HTTP" = "403" ]; then
    echo "    HTTP ${RED}$DENY_HTTP${RST}  →  $DENY_MSG"
    echo "    ${GRN}✓ Rejected at submission. No plan built. No WASM loaded.${RST}"
else
    echo "    HTTP $DENY_HTTP  →  $DENY_MSG"
    echo "    ${RED}✗ Expected 403 but got $DENY_HTTP${RST}"
fi
echo ""

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

echo "  ${BLD}2. Step-level deny${RST} ${DIM}— OPA blocks execution at runtime${RST}"
submit_and_poll "Shell: cat /etc/passwd (path outside allowed_paths)" \
    "run cat /etc/passwd" "" "skill-demo"

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

echo "  ${BLD}3. Content-aware deny${RST} ${DIM}— jailbreak detection in email content${RST}"
echo "  Pipeline: email-read → ${RED}OPA jailbreak gate${RST} → responder → email-send"
echo ""

# The mock inbox email at index 2 contains a prompt injection attempt.
# OPA inspects the email-read output before the responder runs.
echo "  → ${BLD}Reply to injected email (prompt injection blocked)${RST}"

JAILBREAK_BODY=$(curl -sf -X POST http://localhost:8080/tasks \
    -H "Content-Type: application/json" \
    -d '{"type":"email-reply","query":"reply to account verification email","context":{"message":"reply to account verification email","reply_to_index":"1","reply_to":"support@legit-saas.com","reply_subject":"Re: Action Required","reply_body":"Thanks, what do I need to do?"}}')
JAILBREAK_TID=$(echo "$JAILBREAK_BODY" | jq -r '.task_id // ""')

if [ -n "$JAILBREAK_TID" ] && [ "$JAILBREAK_TID" != "null" ]; then
    local_start=$SECONDS
    printf "    ${DIM}⏱  Running (%s) " "$LLM_LABEL"
    JB_STATUS="" JB_STATE="" jb_i=0
    while [ $jb_i -lt 90 ]; do
        JB_STATE=$(curl -sf "http://localhost:8080/tasks/${JAILBREAK_TID}" || echo '{}')
        JB_STATUS=$(echo "$JB_STATE" | jq -r '.status // "unknown"')
        [ "$JB_STATUS" = "completed" ] || [ "$JB_STATUS" = "failed" ] && break
        printf "·"
        sleep 1
        jb_i=$((jb_i + 1))
    done
    local_elapsed=$(( SECONDS - local_start ))
    printf " %ds${RST}\n" "$local_elapsed"

    if [ "$JB_STATUS" = "failed" ]; then
        JB_ERR=$(echo "$JB_STATE" | jq -r '.error // "unknown"' 2>/dev/null)
        echo "    ${RED}✗ Denied:${RST} $JB_ERR"
    else
        echo "    ${GRN}✓ $JB_STATUS${RST}"
    fi
    show_lifecycle "$JAILBREAK_TID"
else
    echo "    ${RED}(submit failed)${RST}"
    echo ""
fi

# ══════════════════════════════════════════════════════════════════════════════
box "HUMAN-IN-THE-LOOP APPROVAL"
echo ""
echo "  OPA policy requires human approval for email-send."
echo "  The orchestrator pauses the step and waits for your decision."
echo ""

echo "  → ${BLD}Send email (requires approval)${RST}"
echo "    ${CYN}▸ send an email to alice@example.com saying meeting moved to 3pm${RST}"

APPR_BODY=$(curl -sf -X POST http://localhost:8080/tasks \
    -H "Content-Type: application/json" \
    -d "$(jq -n '{type:"skill-demo",query:"send an email to alice@example.com saying meeting moved to 3pm",context:{message:"send an email to alice@example.com saying meeting moved to 3pm"}}')")

APPR_TID=$(echo "$APPR_BODY" | jq -r '.task_id // ""')
if [ -z "$APPR_TID" ] || [ "$APPR_TID" = "null" ]; then
    echo "    ${RED}(submit failed: $APPR_BODY)${RST}"
else
    # Poll until a step enters awaiting_approval (or task completes/fails).
    printf "    ${DIM}⏱  Waiting for approval gate "
    APPR_STEP_ID="" APPR_REASON="" appr_i=0
    while [ $appr_i -lt 60 ]; do
        APPR_STATE=$(curl -sf "http://localhost:8080/tasks/${APPR_TID}" || echo '{}')
        APPR_STATUS=$(echo "$APPR_STATE" | jq -r '.status // "unknown"')

        # Check if any step is awaiting approval.
        APPR_STEP_ID=$(echo "$APPR_STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].id // ""' 2>/dev/null)
        if [ -n "$APPR_STEP_ID" ] && [ "$APPR_STEP_ID" != "null" ]; then
            break
        fi

        # Task may have completed or failed before hitting the gate.
        if [ "$APPR_STATUS" = "completed" ] || [ "$APPR_STATUS" = "failed" ]; then
            break
        fi

        printf "·"
        sleep 1
        appr_i=$((appr_i + 1))
    done
    printf "${RST}\n"

    if [ -n "$APPR_STEP_ID" ] && [ "$APPR_STEP_ID" != "null" ]; then
        APPR_AGENT=$(echo "$APPR_STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].agent_type // ""' 2>/dev/null)
        APPR_REASON=$(echo "$APPR_STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].approval_reason // ""' 2>/dev/null)

        echo ""
        echo "    ${YLW}⏸  Step paused: ${BLD}${APPR_AGENT}${RST}${YLW} (${APPR_STEP_ID})${RST}"
        echo "    ${YLW}   Reason: ${APPR_REASON}${RST}"
        echo ""

        printf "    ${BLD}Approve? [y/N]:${RST} "
        read -r APPR_CHOICE </dev/tty

        if [ "$APPR_CHOICE" = "y" ] || [ "$APPR_CHOICE" = "Y" ]; then
            echo ""
            echo "    ${GRN}Approving...${RST}"
            curl -sf -X POST "http://localhost:8080/tasks/${APPR_TID}/steps/${APPR_STEP_ID}/approve" \
                -H "Content-Type: application/json" \
                -d '{"approved_by":"demo-operator"}' > /dev/null

            # Poll for task completion after approval.
            printf "    ${DIM}⏱  Resuming "
            appr_j=0
            while [ $appr_j -lt 60 ]; do
                APPR_STATE=$(curl -sf "http://localhost:8080/tasks/${APPR_TID}" || echo '{}')
                APPR_STATUS=$(echo "$APPR_STATE" | jq -r '.status // "unknown"')
                [ "$APPR_STATUS" = "completed" ] || [ "$APPR_STATUS" = "failed" ] && break
                printf "·"
                sleep 1
                appr_j=$((appr_j + 1))
            done
            printf "${RST}\n"

            if [ "$APPR_STATUS" = "completed" ]; then
                echo "    ${GRN}▹${RST} $(_extract_response "$APPR_STATE")"
            elif [ "$APPR_STATUS" = "failed" ]; then
                APPR_ERR=$(echo "$APPR_STATE" | jq -r '.error // "unknown"' 2>/dev/null)
                echo "    ${RED}✗ Failed:${RST} $APPR_ERR"
            fi
        else
            echo ""
            echo "    ${RED}Rejecting...${RST}"
            curl -sf -X POST "http://localhost:8080/tasks/${APPR_TID}/steps/${APPR_STEP_ID}/reject" \
                -H "Content-Type: application/json" \
                -d '{"rejected_by":"demo-operator","reason":"operator declined in demo"}' > /dev/null

            sleep 1
            APPR_STATE=$(curl -sf "http://localhost:8080/tasks/${APPR_TID}" || echo '{}')
            echo "    ${RED}✗ Step rejected — email not sent.${RST}"
        fi

        echo ""
        show_lifecycle "$APPR_TID"
    elif [ "$APPR_STATUS" = "completed" ] || [ "$APPR_STATUS" = "failed" ]; then
        echo "    ${DIM}(task finished before approval gate — approval may be disabled in policy)${RST}"
        show_lifecycle "$APPR_TID"
    else
        echo "    ${YLW}⚠ Timed out waiting for approval gate${RST}"
        echo ""
    fi
fi

# ══════════════════════════════════════════════════════════════════════════════
box "TAINT TRACKING"
echo ""
echo "  Data provenance labels propagate through the DAG. OPA policy gates"
echo "  actions based on whether tainted data is present in a step's context."
echo ""

# ── Taint helpers ────────────────────────────────────────────────────────────

submit_generic() {
    local steps_json="$1" message="$2"
    curl -sf -X POST http://localhost:8080/tasks \
        -H "Content-Type: application/json" \
        -d "$(jq -n \
            --arg m "$message" \
            --arg s "$steps_json" \
            '{type:"taint-demo", query:$m, context:{message:$m, steps:$s}}')" \
        | jq -r '.task_id // ""'
}

_TAINT_POLL_STATUS=""
_TAINT_POLL_STATE=""
taint_poll() {
    local tid="$1" max="${2:-$DEFAULT_POLL_TIMEOUT}"
    _TAINT_POLL_STATUS="" _TAINT_POLL_STATE=""
    local i=0
    while [ $i -lt "$max" ]; do
        _TAINT_POLL_STATE=$(curl -sf "http://localhost:8080/tasks/$tid" || echo '{}')
        _TAINT_POLL_STATUS=$(echo "$_TAINT_POLL_STATE" | jq -r '.status // "unknown"')
        case "$_TAINT_POLL_STATUS" in
            completed|failed|awaiting_approval) break ;;
        esac
        printf "·"
        sleep 1
        i=$((i + 1))
    done
}

show_taint_map() {
    local tid="$1"
    local taint
    taint=$(curl -sf "http://localhost:8080/tasks/$tid" \
        | jq -r '.taint // {} | to_entries[] | "      \(.key): [\(.value | join(", "))]"' 2>/dev/null)
    if [ -n "$taint" ]; then
        echo "    ${DIM}Taint map:${RST}"
        echo "$taint"
    else
        echo "    ${DIM}Taint map: (empty)${RST}"
    fi
    echo ""
}

# ── 1. Taint blocks shell ────────────────────────────────────────────────────

echo "  ${BLD}1. Taint blocks shell${RST}"
echo "  DAG: web-search → shell"
echo "  web-search output is tainted [web]; shell is ${RED}DENIED${RST} by policy."
echo ""

TAINT_STEPS1='[{"agent_type":"web-search","params":{"query":"WebAssembly security"}},{"agent_type":"shell","depends_on":["0"],"params":{"command":"echo results"}}]'

echo "  → ${BLD}Submitting: web-search → shell${RST}"
TAINT_TID1=$(submit_generic "$TAINT_STEPS1" "search then echo")
if [ -z "$TAINT_TID1" ] || [ "$TAINT_TID1" = "null" ]; then
    echo "    ${RED}(submit failed)${RST}"
else
    echo "    Task: ${DIM}$TAINT_TID1${RST}"
    printf "    ${DIM}⏱  Running (%s) " "$LLM_LABEL"
    taint_poll "$TAINT_TID1"
    printf " ${RST}\n"

    if [ "$_TAINT_POLL_STATUS" = "failed" ]; then
        ERR=$(echo "$_TAINT_POLL_STATE" | jq -r '.error // "unknown"')
        echo "    ${RED}✗ Task failed:${RST} $ERR"
        echo "    ${GRN}^ Expected — shell step was denied by taint policy.${RST}"
    else
        echo "    Status: $_TAINT_POLL_STATUS"
    fi
    echo ""
    show_taint_map "$TAINT_TID1"
    show_lifecycle "$TAINT_TID1"
fi

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

# ── 2. Taint triggers LLM approval gate ──────────────────────────────────────

echo "  ${BLD}2. Taint triggers LLM approval gate${RST}"
echo "  DAG: web-search → responder"
echo "  web-search output is tainted [web]; responder uses llm_complete"
echo "  → ${YLW}APPROVAL REQUIRED${RST} (taint_gates_enabled + web taint + LLM)."
echo ""

TAINT_STEPS2='[{"agent_type":"web-search","params":{"query":"latest Rust programming news"}},{"agent_type":"responder","depends_on":["0"]}]'

echo "  → ${BLD}Submitting: web-search → responder${RST}"
TAINT_TID2=$(submit_generic "$TAINT_STEPS2" "summarize what you found about Rust")
if [ -z "$TAINT_TID2" ] || [ "$TAINT_TID2" = "null" ]; then
    echo "    ${RED}(submit failed)${RST}"
else
    echo "    Task: ${DIM}$TAINT_TID2${RST}"
    printf "    ${DIM}⏱  Running (%s) " "$LLM_LABEL"
    taint_poll "$TAINT_TID2"
    printf " ${RST}\n"

    if [ "$_TAINT_POLL_STATUS" = "awaiting_approval" ]; then
        TAINT_APPR_STEP=$(echo "$_TAINT_POLL_STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].id // ""')
        TAINT_APPR_AGENT=$(echo "$_TAINT_POLL_STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].agent_type // ""')
        TAINT_APPR_REASON=$(echo "$_TAINT_POLL_STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].approval_reason // ""')

        echo ""
        echo "    ${YLW}⏸  Step paused: ${BLD}${TAINT_APPR_AGENT}${RST}${YLW} (${TAINT_APPR_STEP})${RST}"
        echo "    ${YLW}   Reason: ${TAINT_APPR_REASON}${RST}"
        echo "    ${GRN}^ Taint gate fired — web data cannot reach LLM without approval.${RST}"
        echo ""

        show_taint_map "$TAINT_TID2"

        printf "    ${BLD}Approve? [y/N]:${RST} "
        read -r TAINT_CHOICE </dev/tty

        if [ "$TAINT_CHOICE" = "y" ] || [ "$TAINT_CHOICE" = "Y" ]; then
            echo ""
            echo "    ${GRN}Approving...${RST}"
            curl -sf -X POST "http://localhost:8080/tasks/$TAINT_TID2/steps/$TAINT_APPR_STEP/approve" \
                -H "Content-Type: application/json" \
                -d '{"approved_by":"demo-operator"}' > /dev/null

            printf "    ${DIM}⏱  Resuming "
            tj=0
            while [ $tj -lt 120 ]; do
                _TAINT_POLL_STATE=$(curl -sf "http://localhost:8080/tasks/$TAINT_TID2" || echo '{}')
                TS2=$(echo "$_TAINT_POLL_STATE" | jq -r '.status // "unknown"')
                [ "$TS2" = "completed" ] || [ "$TS2" = "failed" ] && break
                printf "·"
                sleep 1
                tj=$((tj + 1))
            done
            printf " ${RST}\n"

            if [ "$TS2" = "completed" ]; then
                TRESP=$(echo "$_TAINT_POLL_STATE" | jq -r '.plan[] | select(.agent_type == "responder") | .output_key' 2>/dev/null)
                if [ -n "$TRESP" ] && [ "$TRESP" != "null" ]; then
                    TRESP_TEXT=$(echo "$_TAINT_POLL_STATE" | jq -r --arg k "$TRESP" '.results[$k] // "{}"' | jq -r '.response // empty' 2>/dev/null)
                    [ -n "$TRESP_TEXT" ] && echo "    ${GRN}▹${RST} $(echo "$TRESP_TEXT" | head -c 400)"
                fi
            elif [ "$TS2" = "failed" ]; then
                echo "    ${RED}✗ Failed after approval${RST}"
            fi
        else
            echo ""
            echo "    ${RED}Rejecting...${RST}"
            curl -sf -X POST "http://localhost:8080/tasks/$TAINT_TID2/steps/$TAINT_APPR_STEP/reject" \
                -H "Content-Type: application/json" \
                -d '{"rejected_by":"demo-operator","reason":"operator declined — tainted data not trusted"}' > /dev/null
            sleep 1
            echo "    ${RED}✗ Step rejected — LLM not invoked with tainted data.${RST}"
        fi
    elif [ "$_TAINT_POLL_STATUS" = "completed" ]; then
        echo "    ${GRN}✓ Completed${RST} (approval gate may not have fired)"
    elif [ "$_TAINT_POLL_STATUS" = "failed" ]; then
        ERR=$(echo "$_TAINT_POLL_STATE" | jq -r '.error // "unknown"')
        echo "    ${RED}✗ Failed:${RST} $ERR"
    fi
    echo ""
    show_lifecycle "$TAINT_TID2"
fi

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

# ── 3. Declassification lifts the gate ───────────────────────────────────────

echo "  ${BLD}3. Declassification lifts the gate${RST}"
echo "  DAG: web-search → summarizer → responder"
echo "  Summarizer has declassifies: [\"web\"] — strips the taint label."
echo "  Responder runs ${GRN}WITHOUT approval${RST} because context_taint is now empty."
echo ""

TAINT_STEPS3='[{"agent_type":"web-search","params":{"query":"WebAssembly component model"}},{"agent_type":"summarizer","depends_on":["0"],"params":{"query":"WebAssembly component model"}},{"agent_type":"responder","depends_on":["1"]}]'

echo "  → ${BLD}Submitting: web-search → summarizer → responder${RST}"
TAINT_TID3=$(submit_generic "$TAINT_STEPS3" "explain the WebAssembly component model")
if [ -z "$TAINT_TID3" ] || [ "$TAINT_TID3" = "null" ]; then
    echo "    ${RED}(submit failed)${RST}"
else
    echo "    Task: ${DIM}$TAINT_TID3${RST}"
    printf "    ${DIM}⏱  Running (%s) " "$LLM_LABEL"
    taint_poll "$TAINT_TID3"
    printf " ${RST}\n"

    if [ "$_TAINT_POLL_STATUS" = "completed" ]; then
        TRESP_KEY=$(echo "$_TAINT_POLL_STATE" | jq -r '.plan[] | select(.agent_type == "responder") | .output_key' 2>/dev/null)
        if [ -n "$TRESP_KEY" ] && [ "$TRESP_KEY" != "null" ]; then
            TRESP3=$(echo "$_TAINT_POLL_STATE" | jq -r --arg k "$TRESP_KEY" '.results[$k] // "{}"' | jq -r '.response // empty' 2>/dev/null)
            [ -n "$TRESP3" ] && echo "    ${GRN}▹${RST} $(echo "$TRESP3" | head -c 400)"
        fi
        echo ""
        echo "    ${GRN}^ No approval gate — summarizer declassified the web taint.${RST}"
    elif [ "$_TAINT_POLL_STATUS" = "awaiting_approval" ]; then
        echo "    ${YLW}⏸  Approval gate fired (unexpected — declassification may not have worked)${RST}"
    elif [ "$_TAINT_POLL_STATUS" = "failed" ]; then
        ERR=$(echo "$_TAINT_POLL_STATE" | jq -r '.error // "unknown"')
        echo "    ${RED}✗ Failed:${RST} $ERR"
    fi
    echo ""
    show_taint_map "$TAINT_TID3"
    show_lifecycle "$TAINT_TID3"
fi

echo "  ${BGRN}Done.${RST}"
