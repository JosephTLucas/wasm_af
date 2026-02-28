#!/usr/bin/env bash
# Shared infrastructure setup for wasmclaw demos.
# Source this file; do not execute it directly.
#
# Provides: build, NATS, orchestrator, gateway startup with cleanup trap.
# After sourcing, the orchestrator is running on :8080 and the gateway on :8081.
# Exports: ROOT, EXAMPLE_DIR, LLM_MODE, LLM_LABEL, ANSI color vars, box(), die(),
#          _lifecycle_lines(), cleanup trap.

[ -n "${_WASMCLAW_SETUP_SOURCED:-}" ] && return 0
_WASMCLAW_SETUP_SOURCED=1

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$ROOT"

[ -f "$HOME/.cargo/env" ] && . "$HOME/.cargo/env"
[ -f "$ROOT/.env" ] && set -a && . "$ROOT/.env" && set +a

LLM_MODE="${LLM_MODE:-mock}"
MODEL="${MODEL:-qwen3:4b}"
NV_API_KEY="${NV_API_KEY:-${LLM_API_KEY:-}}"
NV_MODEL="${NV_MODEL:-meta/llama-3.3-70b-instruct}"

# ── ANSI colors (disabled when piped or NO_COLOR is set) ─────────────────────
if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
    RST=$'\033[0m'  BLD=$'\033[1m'  DIM=$'\033[2m'
    RED=$'\033[31m'  GRN=$'\033[32m'  YLW=$'\033[33m'  CYN=$'\033[36m'
    BRED=$'\033[1;31m'  BGRN=$'\033[1;32m'  BCYN=$'\033[1;36m'
else
    RST=""  BLD=""  DIM=""
    RED=""  GRN=""  YLW=""  CYN=""
    BRED=""  BGRN=""  BCYN=""
fi

case "$LLM_MODE" in
    api)  LLM_LABEL="API" ;;
    real) LLM_LABEL="Ollama" ;;
    *)    LLM_LABEL="Mock" ;;
esac

box() {
    echo "  ${BCYN}╔══════════════════════════════════════════════════════╗${RST}"
    printf "  ${BCYN}║${RST}  ${BLD}%-52s${RST}${BCYN}║${RST}\n" "$1"
    echo "  ${BCYN}╚══════════════════════════════════════════════════════╝${RST}"
}

die() { echo "  ${BRED}ERROR:${RST} $1" >&2; exit 1; }

EXAMPLE_DIR="$ROOT/examples/wasmclaw"
ORCH_PID=""
GW_PID=""
NATS_PID=""
NATS_STARTED_BY_US=""
NATS_STORE_DIR=""
OLLAMA_PID=""
OLLAMA_STARTED_BY_US=""

cleanup() {
    [ -n "$GW_PID" ]   && kill "$GW_PID"   2>/dev/null || true
    [ -n "$ORCH_PID" ] && kill "$ORCH_PID" 2>/dev/null || true
    [ -n "$OLLAMA_STARTED_BY_US" ] && [ -n "$OLLAMA_PID" ] && \
        kill "$OLLAMA_PID" 2>/dev/null || true
    if [ -n "$NATS_STARTED_BY_US" ] && [ -n "$NATS_PID" ]; then
        kill "$NATS_PID" 2>/dev/null || true
        [ -n "$NATS_STORE_DIR" ] && rm -rf "$NATS_STORE_DIR" 2>/dev/null || true
    fi
}
trap cleanup EXIT

_lifecycle_lines() {
    local tid="$1"
    grep "$tid" /tmp/wasm-af-orchestrator.log 2>/dev/null \
        | jq -r --arg g "$GRN" --arg r "$RED" --arg y "$YLW" --arg d "$DIM" --arg R "$RST" --arg b "$BLD" \
          'select(.msg == "starting step" or .msg == "plugin created" or .msg == "plugin destroyed" or .msg == "step completed" or .msg == "step denied" or .msg == "step awaiting approval" or .msg == "step approved" or .msg == "task parked, awaiting approval") |
          if .msg == "starting step" then
            "      \($d)\(.time[11:23])\($R)  \($b)▶\($R) \(.agent_type)"
          elif .msg == "plugin created" then
            "      \($d)\(.time[11:23])    ↳ wasm created  (host_fns: \(.host_fns), load: \(.create_ms)ms)\($R)"
          elif .msg == "plugin destroyed" then
            "      \($d)\(.time[11:23])    ↳ wasm destroyed (exec: \(.exec_ms)ms)\($R)"
          elif .msg == "step denied" then
            "      \($d)\(.time[11:23])\($R)  \($r)✗ \(.agent_type // "step") denied\($R)"
          elif .msg == "step completed" then
            "      \($d)\(.time[11:23])\($R)  \($g)✓ \(.agent_type)\($R)"
          elif .msg == "step awaiting approval" then
            "      \($d)\(.time[11:23])\($R)  \($y)⏸ \(.agent_type // "step") awaiting approval\($R) \($d)(\(.reason // ""))\($R)"
          elif .msg == "step approved" then
            "      \($d)\(.time[11:23])\($R)  \($g)✓ approved\($R) \($d)(by \(.approved_by // "unknown"))\($R)"
          elif .msg == "task parked, awaiting approval" then
            "      \($d)\(.time[11:23])    ↳ task parked — waiting for human decision\($R)"
          else empty end' 2>/dev/null || true
}

# ── 1. Build ──────────────────────────────────────────────────────────────────
echo "  ${BLD}[1/4]${RST} Building..."

if ! rustup target list --installed 2>/dev/null | grep -q wasm32-unknown-unknown; then
    echo "        Adding wasm32-unknown-unknown target..."
    rustup target add wasm32-unknown-unknown || die "Failed to add wasm32-unknown-unknown target."
fi
if ! rustup target list --installed 2>/dev/null | grep -q wasm32-wasip1; then
    echo "        Adding wasm32-wasip1 target..."
    rustup target add wasm32-wasip1 || die "Failed to add wasm32-wasip1 target."
fi

(cd components && cargo build --release -p router -p shell -p memory -p responder -p sandbox-exec -p email-send -p email-read -p web-search 2>&1) \
    || die "Rust build failed (unknown-unknown agents)."
(cd components && cargo build --release -p file-ops --target wasm32-wasip1 2>&1) \
    || die "Rust build failed (file-ops)."
cp components/target/wasm32-wasip1/release/file_ops.wasm \
    components/target/wasm32-unknown-unknown/release/file_ops.wasm

RUNTIMES_DIR="$ROOT/runtimes"
if [ ! -f "$RUNTIMES_DIR/python.wasm" ]; then
    echo "        Downloading sandbox runtimes..."
    bash "$RUNTIMES_DIR/build.sh" 2>&1 | sed 's/^/        /' || \
        echo "        (sandbox runtime download failed — sandbox-exec will be disabled)"
fi

go build -o ./bin/orchestrator ./provider/orchestrator/ 2>&1 || die "Go orchestrator build failed."
go build -o ./bin/webhook-gateway ./cmd/webhook-gateway/ 2>&1 || die "Go gateway build failed."
echo "        ${GRN}Done.${RST}"
echo ""

# ── 2. LLM backend ───────────────────────────────────────────────────────────
if [ "$LLM_MODE" = "api" ]; then
    echo "  ${BLD}[2/4]${RST} LLM backend: ${CYN}API${RST} (NVIDIA NIM)"
    [ -n "$NV_API_KEY" ] || die "LLM_MODE=api requires NV_API_KEY (or LLM_API_KEY). Export it or add to .env"
    echo "        Model: ${CYN}$NV_MODEL${RST}"
elif [ "$LLM_MODE" = "real" ]; then
    echo "  ${BLD}[2/4]${RST} LLM backend: ${CYN}Ollama${RST} ($MODEL)"
    command -v ollama >/dev/null 2>&1 || die "ollama not found. Install from https://ollama.com"
    ollama show "$MODEL" >/dev/null 2>&1 || die "$MODEL not pulled. Run: make pull-model"
    if ! curl -sf http://localhost:11434/api/tags >/dev/null 2>&1; then
        echo "        Starting ollama serve..."
        ollama serve > /tmp/wasm-af-ollama.log 2>&1 &
        OLLAMA_PID=$!
        OLLAMA_STARTED_BY_US="1"
        sleep 3
        curl -sf http://localhost:11434/api/tags >/dev/null 2>&1 || \
            { cat /tmp/wasm-af-ollama.log; die "Ollama failed to start."; }
    fi
    echo "        ${GRN}Ollama ready.${RST}"
else
    echo "  ${BLD}[2/4]${RST} LLM backend: ${YLW}Mock${RST} ${DIM}(set LLM_MODE=api or LLM_MODE=real)${RST}"
fi
echo ""

# ── 3. NATS ───────────────────────────────────────────────────────────────────
echo "  ${BLD}[3/4]${RST} Starting NATS..."

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
    echo "        ${GRN}NATS started${RST} ${DIM}(PID $NATS_PID)${RST}"
fi
echo ""

# ── 4. Services ───────────────────────────────────────────────────────────────
echo "  ${BLD}[4/4]${RST} Starting services..."

rm -rf /tmp/wasmclaw
mkdir -p /tmp/wasmclaw

if lsof -ti:8080 >/dev/null 2>&1; then
    echo "        Stopping stale process on :8080..."
    lsof -ti:8080 | xargs kill 2>/dev/null || true
    sleep 1
fi

if [ "$LLM_MODE" = "api" ]; then
    _LLM_BASE_URL="${LLM_BASE_URL:-https://integrate.api.nvidia.com/v1}"
    _LLM_API_KEY="$NV_API_KEY"
    _LLM_MODEL="$NV_MODEL"
    _LLM_TEMPERATURE="${LLM_TEMPERATURE:-0.2}"
    _LLM_TOP_P="${LLM_TOP_P:-0.7}"
else
    _LLM_BASE_URL="${LLM_BASE_URL:-http://localhost:11434}"
    _LLM_API_KEY="${LLM_API_KEY:-}"
    _LLM_MODEL="$MODEL"
    _LLM_TEMPERATURE="${LLM_TEMPERATURE:-}"
    _LLM_TOP_P="${LLM_TOP_P:-}"
fi

_PLUGIN_TIMEOUT="${PLUGIN_TIMEOUT_SEC:-30}"
if [ "$LLM_MODE" = "api" ] && [ "$_PLUGIN_TIMEOUT" -le 30 ]; then
    _PLUGIN_TIMEOUT=120
fi

OPA_POLICY="$EXAMPLE_DIR" \
OPA_DATA="$EXAMPLE_DIR/data.json" \
AGENT_REGISTRY_FILE="$EXAMPLE_DIR/agents.json" \
LLM_MODE="$LLM_MODE" \
LLM_BASE_URL="$_LLM_BASE_URL" \
LLM_API_KEY="$_LLM_API_KEY" \
LLM_MODEL="$_LLM_MODEL" \
LLM_TEMPERATURE="$_LLM_TEMPERATURE" \
LLM_TOP_P="$_LLM_TOP_P" \
PLUGIN_TIMEOUT_SEC="$_PLUGIN_TIMEOUT" \
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
echo "        ${GRN}Orchestrator running${RST} ${DIM}(LLM: $LLM_LABEL)${RST}"

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
echo "        ${GRN}Webhook gateway running${RST} ${DIM}(PID $GW_PID)${RST}"
echo ""
