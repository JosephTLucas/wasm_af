#!/usr/bin/env bash
# reply-all-demo — Parallel DAG with jailbreak detection + human approval
#
# Demonstrates the DAG scheduler processing two emails in parallel:
#   Email 0 (alice): clean → responder drafts reply → email-send pauses for approval
#   Email 1 (support): prompt injection → responder DENIED by jailbreak gate
#
# Usage:
#   make reply-all-demo
#   LLM_MODE=api make reply-all-demo
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
source "$SCRIPT_DIR/lib/setup.sh"

API="http://localhost:8080"

echo ""
box "REPLY-ALL: Parallel DAG Demo"
echo ""
echo "  Two emails in the inbox:"
echo "    ${GRN}[0]${RST} alice@example.com — Q3 Planning Document ${DIM}(clean)${RST}"
echo "    ${RED}[1]${RST} support@legit-saas.com — Action Required ${DIM}(prompt injection)${RST}"
echo ""
echo "  DAG shape:"
echo ""
echo "                    email-read"
echo "                   /          \\"
echo "          ${GRN}responder-0${RST}       ${RED}responder-1${RST}"
echo "          ${DIM}(email 0)${RST}         ${DIM}(email 1)${RST}"
echo "              |                  |"
echo "        ${YLW}email-send-0${RST}     email-send-1"
echo "        ${DIM}(approval)${RST}       ${DIM}(never runs)${RST}"
echo ""
echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

# ── Submit the reply-all task ─────────────────────────────────────────────────
echo "  → ${BLD}Submitting reply-all task...${RST}"

BODY=$(curl -sf -X POST "$API/tasks" \
    -H "Content-Type: application/json" \
    -d '{
        "type": "reply-all",
        "query": "reply to all emails in my inbox",
        "context": {
            "message": "reply to all emails in my inbox",
            "email_count": "2",
            "reply_to_0": "alice@example.com",
            "reply_subject_0": "Re: Q3 Planning Document",
            "reply_body_0": "Thanks for sharing. I will review the document and follow up by Friday.",
            "reply_to_1": "support@legit-saas.com",
            "reply_subject_1": "Re: Action Required",
            "reply_body_1": "Thanks, what do I need to do?"
        }
    }')

TID=$(echo "$BODY" | jq -r '.task_id // ""')
if [ -z "$TID" ] || [ "$TID" = "null" ]; then
    die "Submit failed: $BODY"
fi
echo "    Task ID: ${DIM}$TID${RST}"
echo ""

# ── Poll until approval gate or terminal state ───────────────────────────────
printf "  ${DIM}⏱  Running DAG "
STEP_ID="" i=0
while [ $i -lt 120 ]; do
    STATE=$(curl -sf "$API/tasks/${TID}" || echo '{}')
    STATUS=$(echo "$STATE" | jq -r '.status // "unknown"')

    STEP_ID=$(echo "$STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].id // ""' 2>/dev/null)
    if [ -n "$STEP_ID" ] && [ "$STEP_ID" != "null" ]; then
        break
    fi

    [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ] && break
    printf "·"
    sleep 1
    i=$((i + 1))
done
printf "${RST}\n"
echo ""

# ── Show what happened so far ─────────────────────────────────────────────────
echo "  ${DIM}Lifecycle so far:${RST}"
_lifecycle_lines "$TID"
echo ""

if [ -z "$STEP_ID" ] || [ "$STEP_ID" = "null" ]; then
    if [ "$STATUS" = "completed" ]; then
        echo "  ${GRN}Task completed${RST} (approval may be disabled in policy)"
    elif [ "$STATUS" = "failed" ]; then
        ERR=$(echo "$STATE" | jq -r '.error // "unknown"')
        echo "  ${RED}Task failed:${RST} $ERR"
    else
        echo "  ${YLW}Timed out waiting for approval gate${RST}"
    fi
    echo ""
    exit 0
fi

# ── Show the approval prompt ──────────────────────────────────────────────────
AGENT=$(echo "$STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].agent_type // ""')
REASON=$(echo "$STATE" | jq -r '[.plan[] | select(.status == "awaiting_approval")][0].approval_reason // ""')

echo "  ${YLW}⏸  Step paused: ${BLD}${AGENT}${RST}${YLW} (${STEP_ID})${RST}"
echo "  ${YLW}   Reason: ${REASON}${RST}"
echo ""
echo "  The jailbreak branch is already dead. This is the clean email branch"
echo "  asking: should I actually send this reply to alice@example.com?"
echo ""

printf "  ${BLD}Approve? [y/N]:${RST} "
read -r CHOICE </dev/tty

if [ "$CHOICE" = "y" ] || [ "$CHOICE" = "Y" ]; then
    echo ""
    echo "  ${GRN}Approving...${RST}"
    curl -sf -X POST "$API/tasks/${TID}/steps/${STEP_ID}/approve" \
        -H "Content-Type: application/json" \
        -d '{"approved_by":"demo-operator"}' > /dev/null

    printf "  ${DIM}⏱  Resuming "
    j=0
    while [ $j -lt 60 ]; do
        STATE=$(curl -sf "$API/tasks/${TID}" || echo '{}')
        STATUS=$(echo "$STATE" | jq -r '.status // "unknown"')
        [ "$STATUS" = "completed" ] || [ "$STATUS" = "failed" ] && break
        printf "·"
        sleep 1
        j=$((j + 1))
    done
    printf "${RST}\n"
    echo ""

    if [ "$STATUS" = "completed" ]; then
        echo "  ${GRN}Task completed.${RST} Clean email sent, injected email blocked."
    elif [ "$STATUS" = "failed" ]; then
        ERR=$(echo "$STATE" | jq -r '.error // "unknown"')
        echo "  ${RED}Task failed:${RST} $ERR"
    fi
else
    echo ""
    echo "  ${RED}Rejecting...${RST}"
    curl -sf -X POST "$API/tasks/${TID}/steps/${STEP_ID}/reject" \
        -H "Content-Type: application/json" \
        -d '{"rejected_by":"demo-operator","reason":"operator declined"}' > /dev/null
    sleep 1
    echo "  ${RED}Step rejected — email not sent.${RST}"
fi

# ── Final lifecycle ───────────────────────────────────────────────────────────
echo ""
echo "  ${DIM}Full lifecycle:${RST}"
_lifecycle_lines "$TID"
echo ""

echo "  ${BGRN}Done.${RST}"
