#!/bin/sh
# Fails if CI's toolchain pins disagree with the Dockerfile builder images.
# CI reads Go from backend/go.mod and Node from frontend/.nvmrc; the image is
# built from the Dockerfile FROM tags. Dependabot bumps only the Dockerfile.
set -eu
cd "$(dirname "$0")/.."

fail=0
mismatch() { # what, file to update, want, got
  echo "$1: Dockerfile builds with $3 but $2 says $4. Update $2 to match." >&2
  fail=1
}

go_image=$(sed -n 's/^FROM golang:\([^@]*\)@.*/\1/p' Dockerfile)
node_image=$(sed -n 's/^FROM node:\([^@-]*\)-slim@.*/\1/p' Dockerfile)
if [ -z "$go_image" ] || [ -z "$node_image" ]; then
  echo "cannot read the golang/node tags from the Dockerfile FROM lines; update this script." >&2
  exit 1
fi
go_mod=$(sed -n 's/^toolchain go//p' backend/go.mod)
[ "$go_image" = "$go_mod" ] || mismatch Go "the toolchain line in backend/go.mod" "go$go_image" "go$go_mod"

# tools/go.mod asking for a newer Go makes `go tool` fetch it, so lint and
# govulncheck would run on a toolchain the image does not ship.
go_tools=$(sed -n 's/^go //p' tools/go.mod)
if [ "$(printf '%s\n%s\n' "$go_tools" "$go_image" | sort -V | tail -n1)" != "$go_image" ]; then
  echo "Go: tools/go.mod requires go$go_tools, newer than the Dockerfile's golang:$go_image. Bump the Dockerfile golang tag and digest, and backend/go.mod's toolchain line." >&2
  fail=1
fi

node_rc=$(tr -d ' \r\n' < frontend/.nvmrc)
[ "$node_image" = "$node_rc" ] || mismatch Node frontend/.nvmrc "$node_image" "$node_rc"
node_engines=$(sed -n 's/^ *"node": "\(.*\)",*$/\1/p' frontend/package.json)
[ "$node_image" = "$node_engines" ] || mismatch Node 'frontend/package.json "engines".node' "$node_image" "$node_engines"

[ "$fail" = 0 ] && echo "toolchains match the Dockerfile: go$go_image, node $node_image"
exit "$fail"
