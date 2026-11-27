#!/usr/bin/env bash
# Builds the browser OpenPGP engine (backend/cmd/pgpwasm) with the exact
# toolchain backend/go.mod pins, and copies that toolchain's wasm_exec.js
# beside it: Go's js/wasm ABI changes between releases, so the loader is
# generated here, never checked in.
# Usage: scripts/build-pgp-wasm.sh [outdir]   (default frontend/.pgp-wasm)
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="${1:-$root/frontend/.pgp-wasm}"
mkdir -p "$out"
cd "$root/backend"
GOTOOLCHAIN="$(awk '$1 == "toolchain" { print $2 }' go.mod)"
export GOTOOLCHAIN
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -trimpath -ldflags=-s -o "$out/kypost-pgp.wasm" ./cmd/pgpwasm
install -m 644 "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$out/wasm_exec.js"
go env GOVERSION > "$out/GOVERSION"
test "$(cat "$out/GOVERSION")" = "$GOTOOLCHAIN"
