#!/usr/bin/env bash
# Download WASI runtimes from high-trust upstream sources.
#
# Supply chain:
#   Python — vmware-labs/webassembly-language-runtimes
#            (VMware, 363+ stars, 32K+ downloads, SHA256 verified)
#            https://github.com/vmware-labs/webassembly-language-runtimes
#
# Usage:
#   ./runtimes/build.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
RUNTIMES_DIR="$SCRIPT_DIR"

PYTHON_VERSION="3.12.0"
PYTHON_RELEASE="python%2F3.12.0%2B20231211-040d5a6"
PYTHON_BASE_URL="https://github.com/vmware-labs/webassembly-language-runtimes/releases/download/${PYTHON_RELEASE}"

echo ""
echo "  ╔══════════════════════════════════════════════════════╗"
echo "  ║        Downloading WASM sandbox runtimes             ║"
echo "  ╚══════════════════════════════════════════════════════╝"
echo ""
echo "  Python ${PYTHON_VERSION} WASI"
echo "    Source: vmware-labs/webassembly-language-runtimes (VMware)"
echo ""

if [ -f "$RUNTIMES_DIR/python.wasm" ]; then
    echo "  python.wasm already present — skipping."
    echo "  (Delete runtimes/python.wasm to re-download.)"
else
    # Download the tarball (includes wasm binary + stdlib).
    TARBALL="python-${PYTHON_VERSION}-wasi-sdk-20.0.tar.gz"
    TARBALL_URL="${PYTHON_BASE_URL}/${TARBALL}"
    CHECKSUM_URL="${TARBALL_URL}.sha256sum"

    echo "  Downloading ${TARBALL}..."
    echo "    ${TARBALL_URL}"
    curl -sL -o "/tmp/${TARBALL}" "$TARBALL_URL"

    echo "  Verifying SHA256 checksum..."
    EXPECTED_SHA=$(curl -sL "$CHECKSUM_URL" | awk '{print $1}')
    ACTUAL_SHA=$(shasum -a 256 "/tmp/${TARBALL}" | awk '{print $1}')
    if [ "$EXPECTED_SHA" != "$ACTUAL_SHA" ]; then
        echo "  ERROR: checksum mismatch!"
        echo "    expected: $EXPECTED_SHA"
        echo "    actual:   $ACTUAL_SHA"
        rm -f "/tmp/${TARBALL}"
        exit 1
    fi
    echo "  Checksum OK: ${ACTUAL_SHA}"

    echo "  Extracting..."
    EXTRACT_DIR=$(mktemp -d)
    tar xzf "/tmp/${TARBALL}" -C "$EXTRACT_DIR"
    rm "/tmp/${TARBALL}"

    # The tarball extracts with a top-level directory; find the .wasm and lib/.
    WASM_FILE=$(find "$EXTRACT_DIR" -name "*.wasm" -type f | head -1)
    LIB_DIR=$(find "$EXTRACT_DIR" -type d -name "lib" | head -1)

    if [ -z "$WASM_FILE" ]; then
        echo "  ERROR: no .wasm file found in tarball"
        rm -rf "$EXTRACT_DIR"
        exit 1
    fi

    cp "$WASM_FILE" "$RUNTIMES_DIR/python.wasm"

    if [ -n "$LIB_DIR" ]; then
        rm -rf "$RUNTIMES_DIR/python-lib"
        cp -R "$LIB_DIR" "$RUNTIMES_DIR/python-lib"
        echo "  Stdlib installed to runtimes/python-lib/"
    fi

    rm -rf "$EXTRACT_DIR"

    SIZE=$(wc -c < "$RUNTIMES_DIR/python.wasm" | tr -d ' ')
    echo "  Installed python.wasm (${SIZE} bytes)"
fi

echo ""
echo "  Runtimes directory:"
ls -lh "$RUNTIMES_DIR"/*.wasm 2>/dev/null | sed 's/^/    /' || echo "    (no .wasm files)"
echo ""
echo "  Done."
