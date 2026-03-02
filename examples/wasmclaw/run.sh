#!/usr/bin/env bash
# wasmclaw — Personal AI Assistant Demo
#
# Usage:
#   make demo                        (recommended — mock LLM)
#   LLM_MODE=api make demo           (NVIDIA NIM API — needs NV_API_KEY)
#   LLM_MODE=real make demo          (local Ollama)
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
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
            "      \($d)\($t[11:23])    ↳ wasm created  (host_fns: \(.fields.host_fns // .host_fns // 0), load: \(.fields.create_ms // .create_ms // 0)ms)\($R)"
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
    printf "    ${DIM}⏱  Inference (%s) " "$LLM_LABEL"
    local STATUS="" STATE="" i=0
    while [ $i -lt "$max_poll" ]; do
        STATE=$(curl -sf "http://localhost:8080/tasks/${TID}" || echo '{}')
        STATUS=$(echo "$STATE" | jq -r '.status // "unknown"')
        [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ] && break
        printf "·"
        sleep 2
        i=$((i + 2))
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
    printf "    ${DIM}⏱  Inference (%s) " "$LLM_LABEL"
    local STATUS="" STATE="" i=0
    while [ $i -lt "$max_poll" ]; do
        STATE=$(curl -sf "http://localhost:8080/tasks/${TID}" || echo '{}')
        STATUS=$(echo "$STATE" | jq -r '.status // "unknown"')
        [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ] && break
        printf "·"
        sleep 2
        i=$((i + 2))
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
    printf "    ${DIM}⏱  Inference (%s) " "$LLM_LABEL"
    JB_STATUS="" JB_STATE="" jb_i=0
    while [ $jb_i -lt 90 ]; do
        JB_STATE=$(curl -sf "http://localhost:8080/tasks/${JAILBREAK_TID}" || echo '{}')
        JB_STATUS=$(echo "$JB_STATE" | jq -r '.status // "unknown"')
        [ "$JB_STATUS" = "completed" ] || [ "$JB_STATUS" = "failed" ] && break
        printf "·"
        sleep 2
        jb_i=$((jb_i + 2))
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
                sleep 2
                appr_j=$((appr_j + 2))
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
box "BINARY CAPABILITY ANALYSIS"
echo ""

WASM_TOOLS=""
if command -v wasm-tools >/dev/null 2>&1; then
    WASM_TOOLS="wasm-tools"
elif [ -x "$HOME/.cargo/bin/wasm-tools" ]; then
    WASM_TOOLS="$HOME/.cargo/bin/wasm-tools"
fi

FILE_OPS_WASM="$ROOT/components/target/wasm32-wasip2/release/file_ops.wasm"
SANDBOX_WASM="$ROOT/components/target/wasm32-wasip2/release/sandbox_exec.wasm"
EMAIL_SEND_WASM="$ROOT/components/target/wasm32-wasip2/release/email_send.wasm"
EMAIL_READ_WASM="$ROOT/components/target/wasm32-wasip2/release/email_read.wasm"

if [ -n "$WASM_TOOLS" ]; then
    echo "  ${BLD}file_ops.wasm${RST} — WIT imports:"
    HOST_IMPORTS=$($WASM_TOOLS print "$FILE_OPS_WASM" 2>/dev/null \
        | grep -E 'wasm-af:agent/' || true)
    if [ -z "$HOST_IMPORTS" ]; then
        echo "    ${GRN}(none)${RST} — uses WASI std::fs only. Wasmtime enforces path boundaries."
    else
        echo "$HOST_IMPORTS" | sed 's/^/    /'
    fi
    echo ""

    echo "  ${BLD}sandbox_exec.wasm${RST} — WIT imports:"
    $WASM_TOOLS print "$SANDBOX_WASM" 2>/dev/null \
        | grep -E 'wasm-af:agent/' | sed 's/^/    /' || \
        echo "    (wasm-tools print failed)"
    echo ""

    echo "  ${BLD}email_send.wasm${RST} — WIT imports:"
    $WASM_TOOLS print "$EMAIL_SEND_WASM" 2>/dev/null \
        | grep -E 'wasm-af:agent/' | sed 's/^/    /' || \
        echo "    (wasm-tools print failed)"
    echo ""

    echo "  ${BLD}email_read.wasm${RST} — WIT imports:"
    EMAIL_READ_IMPORTS=$($WASM_TOOLS print "$EMAIL_READ_WASM" 2>/dev/null \
        | grep -E 'wasm-af:agent/' || true)
    if [ -z "$EMAIL_READ_IMPORTS" ]; then
        echo "    ${GRN}(none)${RST}"
    else
        echo "$EMAIL_READ_IMPORTS" | sed 's/^/    /'
    fi
else
    echo "  ${YLW}(wasm-tools not found — binary analysis skipped)${RST}"
    echo "  Install: cargo install wasm-tools"
fi
echo ""

echo "  ${BGRN}Done.${RST}"
