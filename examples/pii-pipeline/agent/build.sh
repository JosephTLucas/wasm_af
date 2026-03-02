#!/usr/bin/env bash
# Build the PII Redactor BYOA agent using componentize-py.
#
# Produces: pii_redactor.wasm (a WASM Component targeting agent-untrusted)
#
# Build methods (in order of preference):
#   1. Native componentize-py (fastest, needs: pip install componentize-py)
#   2. Docker (works on macOS 26+ where native crashes due to Mach port guards)
#   3. Skip if pii_redactor.wasm already exists
#
# Usage:
#   ./examples/pii-pipeline/agent/build.sh
#   BUILD_METHOD=docker ./examples/pii-pipeline/agent/build.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
WIT_DIR="$ROOT/wit"
OUTPUT="$SCRIPT_DIR/pii_redactor.wasm"

cd "$SCRIPT_DIR"

echo ""
echo "  Building PII Redactor BYOA agent"
echo "  ─────────────────────────────────"

# ── Skip if already built ─────────────────────────────────────────────────────
if [ -f "$OUTPUT" ] && [ -z "${FORCE_REBUILD:-}" ]; then
    echo "  pii_redactor.wasm already present — skipping."
    echo "  (Set FORCE_REBUILD=1 to rebuild, or delete the file.)"
    ls -lh "$OUTPUT" | awk '{print "  Size: "$5}'
    echo ""
    exit 0
fi

# ── Determine build method ────────────────────────────────────────────────────
BUILD_METHOD="${BUILD_METHOD:-auto}"

if [ "$BUILD_METHOD" = "auto" ]; then
    # Try native first, fall back to Docker.
    # Common pip user-install paths (macOS / Linux)
    for _p in "$HOME/Library/Python/3.9/bin" "$HOME/Library/Python/3.12/bin" \
              "$HOME/Library/Python/3.11/bin" "$HOME/Library/Python/3.10/bin" \
              "$HOME/.local/bin"; do
        [ -d "$_p" ] && export PATH="$PATH:$_p"
    done

    if command -v componentize-py >/dev/null 2>&1; then
        # Quick smoke test: if componentize-py crashes (e.g. macOS 26 Mach port
        # guard issue), fall back to Docker.
        if componentize-py --version >/dev/null 2>&1; then
            BUILD_METHOD="native"
        else
            echo "  componentize-py found but not functional — falling back to Docker."
            BUILD_METHOD="docker"
        fi
    elif command -v docker >/dev/null 2>&1; then
        BUILD_METHOD="docker"
    else
        echo "  ERROR: Neither componentize-py nor docker found."
        echo ""
        echo "  Option A: pip3 install componentize-py"
        echo "  Option B: install Docker (https://docker.com)"
        exit 1
    fi
fi

echo "  Build method: $BUILD_METHOD"
echo "  WIT:          $WIT_DIR/agent.wit"
echo "  World:        agent-untrusted"
echo ""

# ── Native build ──────────────────────────────────────────────────────────────
if [ "$BUILD_METHOD" = "native" ]; then
    echo "  Generating Python bindings..."
    componentize-py -d "$WIT_DIR" -w agent-untrusted bindings . 2>&1 | sed 's/^/    /'

    echo "  Compiling WASM component..."
    componentize-py -d "$WIT_DIR" -w agent-untrusted componentize app -o "$OUTPUT" 2>&1 | sed 's/^/    /'

# ── Docker build ──────────────────────────────────────────────────────────────
elif [ "$BUILD_METHOD" = "docker" ]; then
    echo "  Building inside Docker container..."

    # Build a minimal image with componentize-py.
    DOCKER_TAG="wasm-af-byoa-builder"

    docker build -q -t "$DOCKER_TAG" -f - "$ROOT" <<'DOCKERFILE'
FROM python:3.12-slim
RUN pip install --no-cache-dir componentize-py
WORKDIR /build
DOCKERFILE

    echo "  Running componentize-py in container..."

    docker run --rm \
        -v "$WIT_DIR:/wit:ro" \
        -v "$SCRIPT_DIR:/agent" \
        -w /agent \
        "$DOCKER_TAG" \
        bash -c '
            rm -rf wit_world componentize_py_async_support componentize_py_types.py poll_loop.py 2>/dev/null
            componentize-py -d /wit -w agent-untrusted bindings . 2>&1
            componentize-py -d /wit -w agent-untrusted componentize app -o pii_redactor.wasm 2>&1
        ' | sed 's/^/    /'

else
    echo "  ERROR: Unknown BUILD_METHOD=$BUILD_METHOD"
    exit 1
fi

# ── Verify ────────────────────────────────────────────────────────────────────
if [ ! -f "$OUTPUT" ]; then
    echo ""
    echo "  ERROR: Build failed — $OUTPUT not produced."
    exit 1
fi

echo ""
echo "  Built: $OUTPUT"
ls -lh "$OUTPUT" | awk '{print "  Size:  "$5}'
echo ""
