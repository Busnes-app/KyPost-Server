#!/usr/bin/env bash
# The suite end-to-end build tag opens multi-tenant startup, so it may appear only in the gate
# files, docs and these checks, and no release path may pass build tags at all
# (KyCalendar docs/superpowers/specs/2026-11-24-multi-tenant-e2e-design.md, Mechanism).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
tag=kye2e
gates=(backend/internal/config/gate_release.go backend/internal/config/gate_kye2e.go)
fail=0

stray=$(git grep -l -i "$tag" -- . ':!backend/internal/config/gate_release.go' ':!backend/internal/config/gate_kye2e.go' \
  ':!backend/internal/*/gate_kye2e_test.go' ':!*.md' ':!scripts/check-test-build-tag.sh' ':!scripts/check-release-image.sh' || true)
if [ -n "$stray" ]; then
  echo "::error::the $tag build tag appears outside the gate files:" >&2
  echo "$stray" >&2
  fail=1
fi

# A -tags (or GOFLAGS) in a release path would let a release build pass the tag without editing
# a gate file. git describe --tags is the only other use of the word there.
mapfile -t release < <(git ls-files -- Makefile '*Makefile' 'Dockerfile*' '*/Dockerfile*' 'docker-compose*.yml' \
  .github/workflows/ci.yml .github/workflows/release-image.yml .github/workflows/publish-main.yml .github/workflows/dependency-release.yml)
if found=$(grep -n -H -E -- '(^|[^[:alnum:]_])-{1,2}tags([=[:space:]"]|$)|GOFLAGS' "${release[@]}" | grep -v 'git describe'); then
  echo "::error::a release path passes build tags:" >&2
  echo "$found" >&2
  fail=1
fi

# Both gate files exist and say what they must.
{ grep -qx '//go:build !kye2e' "${gates[0]}" && grep -qx 'const MultiTenantBuild = false' "${gates[0]}"; } ||
  { echo "::error::${gates[0]} must be //go:build !$tag with MultiTenantBuild = false" >&2; fail=1; }
grep -qx '//go:build kye2e' "${gates[1]}" ||
  { echo "::error::${gates[1]} must be //go:build $tag" >&2; fail=1; }
[ "$fail" -eq 0 ] || exit 1

# The test build opens the gate and takes only a .test issuer (the untagged suite checks that
# the release build refuses).
cd backend
go vet -tags "$tag" ./internal/config/ ./internal/app/ ./internal/sso/
go test -count=1 -tags "$tag" -run '^TestE2EBuild' ./internal/config/ ./internal/app/ ./internal/sso/
