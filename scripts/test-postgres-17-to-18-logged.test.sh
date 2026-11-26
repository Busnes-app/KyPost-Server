#!/usr/bin/env bash
# Checks test-postgres-17-to-18.sh's `logged` log assertion on a stream that keeps writing
# after its marker: the shape that made `logs | grep -q` fail under pipefail (GNU grep; SIGPIPE
# 141, or 1 where SIGPIPE is ignored) on a check whose marker was there. Runs the script's own
# `logged`; needs no Docker.
set -Eeuo pipefail
cd "$(dirname "$0")/.."
script=scripts/test-postgres-17-to-18.sh
eval "$(sed -n '/^logged() {$/,/^}$/p' "$script")"
declare -F logged >/dev/null || { echo "FAIL: no logged() in $script" >&2; exit 1; }
fail() { echo "logged-fail: $*"; exit 3; } # what the real script's fail does: end the run

# The marker first, then far more than a pipe buffer, written after grep could have stopped.
stream() { echo "postgres-17-to-18: restored /x/dump.sql"; sleep 0.2; head -c 1048576 /dev/zero | tr '\0' 'x'; echo; }
missing() { echo "nothing here"; head -c 1048576 /dev/zero | tr '\0' 'x'; echo; }
broken() { echo "restored"; return 7; } # a failing `docker compose logs`

problems=0
check() { if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: got $2, want $3"; problems=$((problems + 1)); fi; }

# The old form fails here: the reason for the change (GNU grep; ugrep drains its input). 141 is
# SIGPIPE; where SIGPIPE is ignored, as on GitHub's runner, the producer's write fails instead (1).
if grep --version 2>/dev/null | head -n1 | grep -q 'GNU grep'; then
  old=$( (stream 2>/dev/null | grep -q restored) && echo 0 || echo $?)
  check "old pipe form fails on an early marker (GNU grep, status $old)" "$([ "$old" != 0 ] && echo fails || echo passes)" fails
fi
got=$(logged restored stream && echo 0 || echo $?)
check "early marker, more output after it" "$got" 0
got=$(logged restored missing && echo 0 || echo $?)
check "marker missing" "$got" 1
got=$( (logged restored broken && echo 0 || echo $?) || true)
check "a failing producer fails the test" "$got" "logged-fail: broken failed"
got=$( (logged restored broken && echo "skipped-fail" || echo $?) || true)
check "a failing producer is not read as not logged" "$got" "logged-fail: broken failed"
! grep -qE '^[^#]*logs [^|]*\| *(tail[^|]*\| *)?grep -q' "$script" || { echo "FAIL: $script pipes logs into grep -q"; problems=$((problems + 1)); }
[ "$problems" = 0 ] && echo "PASS logged: early marker, missing marker, failing producer" || exit 1
