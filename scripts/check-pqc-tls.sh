#!/usr/bin/env bash
# Keep Go's default hybrid post-quantum TLS key exchange (X25519MLKEM768) active
# everywhere: non-test Go code must not set CurvePreferences (use ky-primitives
# pqtls.HybridOnly for Ky-to-Ky links that require hybrid-only), and nothing in
# code, builds, Compose or workflows may override GODEBUG tlsmlkem.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

fail=0

bad_curves=$(git grep -n 'CurvePreferences' -- '*.go' ':!*_test.go' \
  | grep -v 'pqc-tls:allow' \
  | awk '{ code = $0; sub(/^[^:]+:[0-9]+:/, "", code); sub(/\/\/.*/, "", code); if (code ~ /CurvePreferences/) print $0 }' || true)
if [ -n "$bad_curves" ]; then
  echo "::error::non-test Go code sets CurvePreferences (use pqtls.HybridOnly or add // pqc-tls:allow with rationale):" >&2
  printf '%s\n' "$bad_curves" >&2
  fail=1
fi

bad_godebug=$(git grep -n -i 'tlsmlkem' -- . ':!*.md' ':!scripts/check-pqc-tls.sh' || true)
if [ -n "$bad_godebug" ]; then
  echo "::error::tlsmlkem GODEBUG override found outside docs:" >&2
  printf '%s\n' "$bad_godebug" >&2
  fail=1
fi

exit "$fail"
