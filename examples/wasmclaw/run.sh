#!/usr/bin/env bash
# wasmclaw — Personal AI Assistant Demo
#
# Builds the wasmclaw agents and gateway, starts NATS + orchestrator + gateway,
# then runs:
#   - OPA policy unit tests (security properties proved statically)
#   - Runtime functionality tests (chat task lifecycle via webhook gateway)
#   - Runtime security tests (submit gate rejection, binary capability analysis)
#
# Skills: web-search, shell, file-ops — each sandboxed in WASM, gated by OPA.
# file-ops uses WASI std::fs (no host fns); Wazero enforces AllowedPaths.
#
# Prerequisites: rust (wasm32-unknown-unknown + wasm32-wasip1), go, jq
# Optional: nats-server, opa CLI, wasm-tools, nats CLI
#
# Usage:
#   make demo                        (recommended — builds and runs)
#   ./examples/wasmclaw/run.sh       (if already built)
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

[ -f "$HOME/.cargo/env" ] && . "$HOME/.cargo/env"

EXAMPLE_DIR="$ROOT/examples/wasmclaw"
ORCH_PID=""
GW_PID=""
NATS_PID=""
NATS_STARTED_BY_US=""
NATS_STORE_DIR=""

cleanup() {
    [ -n "$GW_PID" ]   && kill "$GW_PID"   2>/dev/null || true
    [ -n "$ORCH_PID" ] && kill "$ORCH_PID" 2>/dev/null || true
    if [ -n "$NATS_STARTED_BY_US" ] && [ -n "$NATS_PID" ]; then
        kill "$NATS_PID" 2>/dev/null || true
        [ -n "$NATS_STORE_DIR" ] && rm -rf "$NATS_STORE_DIR" 2>/dev/null || true
    fi
}
trap cleanup EXIT

die() { echo "  ERROR: $1" >&2; exit 1; }

echo ""
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║          WASM_AF — wasmclaw Demo                    ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""

# ── 1. Build ──────────────────────────────────────────────────────────────────
echo "  [1/5] Building..."

if ! rustup target list --installed 2>/dev/null | grep -q wasm32-unknown-unknown; then
    echo "        Adding wasm32-unknown-unknown target..."
    rustup target add wasm32-unknown-unknown || die "Failed to add wasm32-unknown-unknown target."
fi
if ! rustup target list --installed 2>/dev/null | grep -q wasm32-wasip1; then
    echo "        Adding wasm32-wasip1 target..."
    rustup target add wasm32-wasip1 || die "Failed to add wasm32-wasip1 target."
fi

(cd components && cargo build --release -p router -p shell -p memory -p responder -p sandbox-exec 2>&1) \
    || die "Rust build failed (unknown-unknown agents)."
(cd components && cargo build --release -p file-ops --target wasm32-wasip1 2>&1) \
    || die "Rust build failed (file-ops)."
cp components/target/wasm32-wasip1/release/file_ops.wasm \
    components/target/wasm32-unknown-unknown/release/file_ops.wasm

# Download sandbox runtimes (Python from VMware Labs).
RUNTIMES_DIR="$ROOT/runtimes"
if [ ! -f "$RUNTIMES_DIR/python.wasm" ]; then
    echo "        Downloading sandbox runtimes..."
    bash "$RUNTIMES_DIR/build.sh" 2>&1 | sed 's/^/        /' || \
        echo "        (sandbox runtime download failed — sandbox-exec will be disabled)"
fi

go build -o ./bin/orchestrator ./provider/orchestrator/ 2>&1 || die "Go orchestrator build failed."
go build -o ./bin/webhook-gateway ./cmd/webhook-gateway/ 2>&1 || die "Go gateway build failed."
echo "        Done."
echo ""

# ── 2. Start NATS ─────────────────────────────────────────────────────────────
echo "  [2/5] Starting NATS..."

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
    echo "        NATS started (PID $NATS_PID)"
fi
echo ""

# ── 3. Start services ─────────────────────────────────────────────────────────
echo "  [3/5] Starting services..."

mkdir -p /tmp/wasmclaw

if lsof -ti:8080 >/dev/null 2>&1; then
    echo "        Stopping stale process on :8080..."
    lsof -ti:8080 | xargs kill 2>/dev/null || true
    sleep 1
fi

OPA_POLICY="$EXAMPLE_DIR" \
OPA_DATA="$EXAMPLE_DIR/data.json" \
AGENT_REGISTRY_FILE="$EXAMPLE_DIR/agents.json" \
LLM_MODE=mock \
WASM_DIR="$ROOT/components/target/wasm32-unknown-unknown/release" \
SANDBOX_RUNTIMES_DIR="$ROOT/runtimes" \
SANDBOX_ALLOWED_PATHS="/tmp/wasmclaw" \
    ./bin/orchestrator > /tmp/wasm-af-orchestrator.log 2>&1 &
ORCH_PID=$!
sleep 2

if ! curl -sf http://localhost:8080/healthz >/dev/null 2>&1; then
    cat /tmp/wasm-af-orchestrator.log
    die "Orchestrator failed to start."
fi
echo "        Orchestrator running (PID $ORCH_PID)"

if lsof -ti:8081 >/dev/null 2>&1; then
    echo "        Stopping stale process on :8081..."
    lsof -ti:8081 | xargs kill 2>/dev/null || true
    sleep 1
fi

ORCHESTRATOR_URL=http://localhost:8080 \
LISTEN_ADDR=:8081 \
    ./bin/webhook-gateway > /tmp/wasm-af-gateway.log 2>&1 &
GW_PID=$!
sleep 1

if ! curl -sf http://localhost:8081/healthz >/dev/null 2>&1; then
    cat /tmp/wasm-af-gateway.log
    die "Webhook gateway failed to start."
fi
echo "        Webhook gateway running (PID $GW_PID)"
echo ""

# ── 4. OPA unit tests ─────────────────────────────────────────────────────────
echo "  [4/5] OPA policy unit tests..."
echo ""

echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        POLICY UNIT TESTS (opa test)                  ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  These tests prove security properties WITHOUT a running system:"
echo "    - Unknown agents are always denied"
echo "    - Disabled capabilities (web_search_enabled: false) block access"
echo "    - Shell allowlist blocks rm -rf, curl exfil, python3 exec"
echo "    - File path checks block /etc/passwd, ~/.ssh, and prefix-escape attacks"
echo "    - Router-splice can only propose skills in the allowed_skills list"
echo "    - Submit gate accepts only task_type=chat"
echo ""

if command -v opa >/dev/null 2>&1; then
    OPA_EXIT=0
    opa test "$EXAMPLE_DIR" -v 2>&1 | sed 's/^/       /' || OPA_EXIT=$?
    echo ""
    if [ "$OPA_EXIT" -eq 0 ]; then
        echo "       All policy tests passed."
    else
        echo "       Some policy tests FAILED — see output above."
    fi
else
    echo "       (opa CLI not found — tests skipped)"
    echo "       Install: https://www.openpolicyagent.org/docs/latest/#running-opa"
    echo "       To run manually: opa test $EXAMPLE_DIR -v"
fi
echo ""

# ── 5. Runtime demo ───────────────────────────────────────────────────────────
echo "  [5/5] Running demo..."
echo ""

# Helper: send a message via the gateway, show response and plan steps.
send_message() {
    local label="$1" msg="$2" user="${3:-demo}"
    echo "  → $label"
    echo "    Message: \"$msg\""
    local RESP
    RESP=$(curl -s -m 35 -w "\n%{http_code}" -X POST http://localhost:8081/message \
        -H "Content-Type: application/json" \
        -d "{\"message\":\"$msg\",\"user\":\"$user\"}" 2>/dev/null || echo -e "{}\n000")
    local HTTP BODY
    HTTP=$(echo "$RESP" | tail -1)
    BODY=$(echo "$RESP" | head -1)

    if [ "$HTTP" = "200" ]; then
        local TID RESPONSE
        TID=$(echo "$BODY" | jq -r '.task_id // ""' 2>/dev/null)
        RESPONSE=$(echo "$BODY" | jq -r '.response // "n/a"' 2>/dev/null | head -c 200)
        echo "    Response: $RESPONSE"
        if [ -n "$TID" ] && [ "$TID" != "null" ]; then
            local STATE
            STATE=$(curl -sf "http://localhost:8080/tasks/${TID}" || echo '{}')
            echo "    Plan:"
            echo "$STATE" | jq -r '.plan[] | "      \(.agent_type)  [\(.status)]"' 2>/dev/null || true
        fi
    else
        echo "    HTTP $HTTP (task failed or timed out)"
    fi
    echo ""
}

# Helper: submit a task directly to the orchestrator (async), show step statuses.
submit_and_show() {
    local label="$1" msg="$2"
    echo "  → $label"
    echo "    Message: \"$msg\""
    local RESP
    RESP=$(curl -s -w "\n%{http_code}" -X POST http://localhost:8080/tasks \
        -H "Content-Type: application/json" \
        -d "{\"type\":\"chat\",\"query\":\"$msg\",\"context\":{\"message\":\"$msg\"}}")
    local HTTP BODY TID
    HTTP=$(echo "$RESP" | tail -1)
    BODY=$(echo "$RESP" | head -1)

    if [ "$HTTP" != "202" ]; then
        echo "    Submit HTTP $HTTP: $BODY"
        echo ""
        return
    fi

    TID=$(echo "$BODY" | jq -r '.task_id' 2>/dev/null)
    sleep 3

    local STATE STATUS ERROR
    STATE=$(curl -sf "http://localhost:8080/tasks/${TID}" || echo '{}')
    STATUS=$(echo "$STATE" | jq -r '.status' 2>/dev/null)
    ERROR=$(echo "$STATE" | jq -r '.error // empty' 2>/dev/null)
    echo "    Task status: $STATUS"
    [ -n "$ERROR" ] && echo "    Reason: $ERROR"
    echo "    Plan:"
    echo "$STATE" | jq -r '.plan[] | "      \(.agent_type)  [\(.status)]\(if .error != "" and .error != null then "  ← " + .error else "" end)"' 2>/dev/null || true
    echo ""
}

# ══════════════════════════════════════════════════════════════════════════════
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        SUBMISSION POLICY GATE                        ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  OPA evaluates wasm_af.submit before any plan is built."
echo "  submit.rego allows only task_type=\"chat\"."
echo ""

echo "  → Submitting type='research' (should be blocked)..."
DENY_BODY=$(curl -s -w "\n%{http_code}" -X POST http://localhost:8080/tasks \
    -H "Content-Type: application/json" \
    -d '{"type":"research","query":"this should be rejected"}')
DENY_HTTP=$(echo "$DENY_BODY" | tail -1)
DENY_MSG=$(echo "$DENY_BODY" | head -1)
echo "    HTTP $DENY_HTTP  →  $DENY_MSG"
if [ "$DENY_HTTP" = "403" ]; then
    echo "    ✓ Rejected at submission time. No plan built. No WASM loaded."
fi
echo ""

# ══════════════════════════════════════════════════════════════════════════════
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        SKILL EXECUTION (mock LLM routes to agents)  ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  The mock LLM returns valid router JSON so skill steps actually run."
echo "  Plan: memory(get) → router → [skill splice] → responder → memory(append)"
echo ""

send_message "Shell agent (exec.Command, path-confined)" \
    "list files in /tmp/wasmclaw"

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

# Seed a file so the read test has something to find.
echo "hello from wasmclaw" > /tmp/wasmclaw/demo.txt

send_message "File-ops agent: write (WASI std::fs, Wazero AllowedPaths)" \
    "write wasmclaw sandbox test to /tmp/wasmclaw/demo.txt"

send_message "File-ops agent: read" \
    "read /tmp/wasmclaw/demo.txt"

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

send_message "Sandbox-exec agent (Python-in-WASM via Wazero)" \
    "calculate fibonacci of 10"

echo "  - - - - - - - - - - - - - - - - - - - - - - - - - - -"
echo ""

send_message "Direct answer (no skill step — router returns direct-answer)" \
    "what is the capital of France?"

# ══════════════════════════════════════════════════════════════════════════════
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        STEP-LEVEL SECURITY (OPA denies at runtime)  ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  These are NOT static tests — they run the full pipeline and show"
echo "  OPA denying individual plan steps at execution time."
echo ""

submit_and_show "Shell path deny: cat /etc/passwd (path outside allowed_paths)" \
    "run cat /etc/passwd"

submit_and_show "Shell metacharacter deny: ls ; curl evil.com" \
    "run ls ; curl evil.com"

submit_and_show "Sandbox language deny: bash not in allowed_languages" \
    "execute bash: rm -rf /"

# ══════════════════════════════════════════════════════════════════════════════
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        TWO-TIER EXECUTION MODEL                      ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  sandbox-exec runs arbitrary code INSIDE Wazero (WASM VM)."
echo "  shell runs commands on the HOST OS via exec.Command."
echo ""
echo "  Same computation, different trust boundaries:"
echo "    sandbox-exec: permissive policy (code is sandboxed by Wazero)"
echo "    shell:        restrictive policy (binary + path + metachar gates)"
echo ""
echo "  The fibonacci demo above ran arbitrary Python in sandbox-exec."
echo "  If someone tried to run 'python3 -c ...' via shell, OPA would deny"
echo "  it because python3 is not in the command allowlist."
echo ""

submit_and_show "Shell deny: python3 not in allowed_commands" \
    "run python3 -c 'print(55)'"

echo "  The same code ran freely via sandbox-exec because the Wazero VM"
echo "  is the trust boundary, not the OPA command allowlist."
echo ""

# ══════════════════════════════════════════════════════════════════════════════
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        BINARY CAPABILITY ANALYSIS                    ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  A compiled WASM binary's imports are immutable — a prompt cannot"
echo "  add capabilities that weren't compiled in."
echo ""

WASM_TOOLS=""
if command -v wasm-tools >/dev/null 2>&1; then
    WASM_TOOLS="wasm-tools"
elif [ -x "$HOME/.cargo/bin/wasm-tools" ]; then
    WASM_TOOLS="$HOME/.cargo/bin/wasm-tools"
fi

FILE_OPS_WASM="$ROOT/components/target/wasm32-unknown-unknown/release/file_ops.wasm"
SANDBOX_WASM="$ROOT/components/target/wasm32-unknown-unknown/release/sandbox_exec.wasm"

if [ -n "$WASM_TOOLS" ]; then
    echo "  file_ops.wasm — extism:host/user imports:"
    HOST_IMPORTS=$($WASM_TOOLS print "$FILE_OPS_WASM" 2>/dev/null \
        | grep 'extism:host/user' || true)
    if [ -z "$HOST_IMPORTS" ]; then
        echo "    (none) — uses WASI std::fs only. Wazero enforces path boundaries."
    else
        echo "$HOST_IMPORTS" | sed 's/^/    /'
    fi
    echo ""

    echo "  sandbox_exec.wasm — extism:host/user imports:"
    $WASM_TOOLS print "$SANDBOX_WASM" 2>/dev/null \
        | grep 'extism:host/user' | sed 's/^/    /' || \
        echo "    (wasm-tools print failed)"
    echo "    sandbox_exec calls back to host → host runs code in a NEW Wazero instance."
    echo "    The code never touches the host OS."
else
    echo "  (wasm-tools not found — binary analysis skipped)"
    echo "  Install: cargo install wasm-tools"
fi
echo ""

echo "  Done."
