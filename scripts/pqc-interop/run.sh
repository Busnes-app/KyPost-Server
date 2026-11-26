#!/usr/bin/env bash
# Which OpenPGP tools accept KyPost's v4 and v6 public certificates, alone and as one file?
# Usage: scripts/pqc-interop/run.sh [image]   (default debian:trixie; needs Go 1.27 and Docker)
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
image=${1:-debian:trixie}
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
(cd "$here/gen" && go run . "$out")
cp "$here/check.sh" "$out/"
docker run --rm -v "$out":/w:ro "$image" bash /w/check.sh
