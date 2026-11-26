#!/usr/bin/env bash
# Builds the gopenpgp WebAssembly engine (backend/cmd/pgpwasm) and copies the
# wasm_exec.js of the SAME Go toolchain beside it: Go's js/wasm ABI changes
# between releases, so the loader is generated here, never checked in.
# Output: frontend/.pgp-wasm/{kypost-pgp.wasm,wasm_exec.js} (gitignored).
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/frontend/.pgp-wasm"
mkdir -p "$out"
cd "$root/backend"
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -trimpath -ldflags=-s -o "$out/kypost-pgp.wasm" ./cmd/pgpwasm
cp "$(go env GOROOT)/lib/wasm/wasm_exec.js" "$out/wasm_exec.js"
go env GOVERSION > "$out/GOVERSION"
ls -l "$out"
