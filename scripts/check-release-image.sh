#!/usr/bin/env bash
# Usage: check-release-image.sh <image>
# Fails unless the image is a release build: its binary carries no end-to-end build tag, and a
# complete multi-tenant configuration still meets the refusal, both at the entrypoint's bootstrap
# and in the server process (KyCalendar docs/superpowers/specs/2026-11-24-multi-tenant-e2e-design.md,
# Mechanism).
set -euo pipefail
image=${1:?usage: check-release-image.sh <image>}
dir=$(mktemp -d)
cid=$(docker create "$image")
run=kypost-release-check-$$
trap 'docker rm -f "$cid" "$run" >/dev/null 2>&1; rm -rf "$dir"' EXIT
docker cp -q "$cid:/usr/local/bin/kypost-server" "$dir/kypost-server"

info=$(go version -m "$dir/kypost-server")
grep -q -P '^\t(mod|dep)\tgithub.com/Busnes-app/kypost-server/backend\t' <<<"$info" ||
  { echo "::error::$image: no build info for kypost-server:" >&2; echo "$info" >&2; exit 1; }
if tags=$(grep -P '^\tbuild\t-tags=' <<<"$info") && grep -q -w kye2e <<<"$tags"; then
  echo "::error::$image was built with the end-to-end test tag: $tags" >&2
  exit 1
fi

# A multi-tenant configuration (tenancy design K2) that only the gate may refuse: the match on
# the gate's own message proves no other check did. --network none, so there is no database:
# KYPOST_DATABASE_URL would make every command connect first (dbbackend.Check), and the gate
# reads only the environment.
multi=(--rm --name "$run" --network none --memory 512m -e KYPOST_BIND=127.0.0.1
  -e KYPOST_TENANCY=multi -e KYPOST_STAFF_ORG_ID=6f1c2b9e-3d4a-4f5b-8c7d-0e1f2a3b4c5d -e KY_CELLS=e2e-1
  -e KYPOST_NATIVE_MAIL=true -e KYPOST_DISABLE_AI_SORTING=true)
want='KYPOST_TENANCY=multi is not available in this release'
for how in entrypoint server; do
  args=("${multi[@]}" "$image")
  [ "$how" = server ] && args=("${multi[@]}" --entrypoint /usr/local/bin/kypost-server "$image" --mode server)
  set +e
  # A refusal exits at once; a build that starts instead is stopped after 60 s.
  out=$(timeout 60 docker run "${args[@]}" 2>&1)
  code=$?
  set -e
  docker rm -f "$run" >/dev/null 2>&1 || true
  if [ "$code" -eq 0 ] || ! grep -qF "$want" <<<"$out"; then
    echo "::error::$image ($how) did not refuse a complete multi-tenant configuration (exit $code):" >&2
    echo "$out" >&2
    exit 1
  fi
done
echo "$image: release build, multi-tenant startup refused"
