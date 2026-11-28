#!/usr/bin/env bash
# Usage: rehearse-cell-promotion.sh <kypost image>
# Rehearses docs/CELL-RECOVERY.md steps 1-4, the step 6 refusal and the step 7 rebuild, on
# disposable containers only: a PostgreSQL 18 primary, a streaming standby, and a single-mode
# KyPost server (multi mode cannot start until the tenancy design's PR 7). Push relays are
# empty. Everything it makes is removed on exit.
set -euo pipefail
image=${1:?usage: rehearse-cell-promotion.sh <kypost image>}
pg=postgres:18.7-trixie@sha256:c699303802144847d238196173655dbbecf7a96aeca1565f2b02eb671a1f0b6b
id=cellrh-$$
net=$id-net work=$(mktemp -d)
cleanup() {
  docker rm -f "$id-primary" "$id-standby" "$id-rebuilt" "$id-kp" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
trap 'exit 1' INT TERM
fail() { echo "FAIL: $*" >&2; exit 1; }
sql() { docker exec "$1" psql -U postgres -Atc "$2"; }
# TCP, not the socket: the image's init runs a socket-only temporary server, then restarts.
ready() { for _ in $(seq 60); do docker exec "$1" pg_isready -U postgres -h 127.0.0.1 -q && return; sleep 1; done; fail "$1 did not start"; }

docker network create "$net" >/dev/null
docker run -d --name "$id-primary" --network "$net" --memory 1g -e POSTGRES_HOST_AUTH_METHOD=trust \
  -e POSTGRES_INITDB_ARGS='--locale=C --encoding=UTF8' "$pg" -c wal_level=replica -c max_wal_senders=5 >/dev/null
ready "$id-primary"
docker exec "$id-primary" bash -c 'echo "host replication all all trust" >> "$PGDATA/pg_hba.conf"'
sql "$id-primary" "SELECT pg_reload_conf()" >/dev/null

standby() { # name, primary
  docker run -d --name "$1" --network "$net" --memory 1g --user postgres --entrypoint bash "$pg" -c \
    "pg_basebackup -h $2 -U postgres -D \"\$PGDATA\" -R -X stream && exec postgres" >/dev/null
  ready "$1"
  [ "$(sql "$1" 'SELECT pg_is_in_recovery()')" = t ] || fail "$1 is not a standby"
}
standby "$id-standby" "$id-primary"

# A KyPost server records the cell's shared settings in the primary, then stops.
mkdir -p "$work/config" "$work/state" "$work/secret"
kypost() { # name, database host, secret dir
  docker run -d --name "$1" --network "$net" --memory 512m --user "$(id -u):$(id -g)" \
    -v "$work/config:/kp/config" -v "$work/state:/kp/state" -v "$3:/kp/secret" \
    -e CONFIG_DIR=/kp/config -e STATE_DIR=/kp/state -e SECRET_DIR=/kp/secret -e LOG_DIR=/kp/state \
    -e KYPOST_DATABASE_URL="postgres://postgres@$2:5432/postgres?sslmode=disable" \
    -e KYPOST_DISABLE_AI_SORTING=true -e PUSH_RELAY_URL= -e APNS_RELAY_URL= -e SERVER_BASE_URL=https://mail.cell.test \
    --entrypoint /usr/local/bin/kypost-server "$image" --mode server >/dev/null
}
recorded() { [ "$(sql "$1" "SELECT count(*) FROM kypost_state.meta WHERE key = 'replica_config_fingerprint'" 2>/dev/null)" = 1 ]; }
kypost "$id-kp" "$id-primary" "$work/secret"
for _ in $(seq 90); do recorded "$id-primary" && break; sleep 1; done
recorded "$id-primary" || { docker logs "$id-kp" | tail -5; fail "KyPost did not record its settings"; }
docker rm -f "$id-kp" >/dev/null

# Step 1: the standby has replayed everything and reports the loss window.
target=$(sql "$id-primary" "SELECT pg_current_wal_lsn()")
for _ in $(seq 60); do
  [ "$(sql "$id-standby" "SELECT pg_last_wal_replay_lsn() >= '$target'")" = t ] && break; sleep 1
done
read -r rec lag <<<"$(sql "$id-standby" "SELECT pg_is_in_recovery(), now() - pg_last_xact_replay_timestamp()" | tr '|' ' ')"
[ "$rec" = t ] || fail "step 1: not in recovery"
echo "PASS step 1: standby in recovery, loss window $lag"

# Step 2: fence the old primary.
docker stop "$id-primary" >/dev/null
echo "PASS step 2: old primary stopped"

# Step 3: promote.
[ "$(sql "$id-standby" "SELECT pg_promote(wait => true)")" = t ] || fail "step 3: promote"
[ "$(sql "$id-standby" "SELECT pg_is_in_recovery()")" = f ] || fail "step 3: still in recovery"
echo "PASS step 3: standby promoted"

# Step 4: the cell's records came with it (single mode has the fingerprint only).
keys=$(sql "$id-standby" "SELECT key FROM kypost_state.meta WHERE key IN ('tenancy_mode', 'staff_org_id', 'replica_config_fingerprint') ORDER BY key")
[ "$keys" = replica_config_fingerprint ] || fail "step 4: records $keys"
echo "PASS step 4: the promoted database holds the cell's record"

# Step 6: the same keys start against the promoted database; another key is refused.
kypost "$id-kp" "$id-standby" "$work/secret"
sleep 15
[ "$(docker inspect -f '{{.State.Running}}' "$id-kp")" = true ] || { docker logs "$id-kp" | tail -5; fail "step 6: same keys refused"; }
docker rm -f "$id-kp" >/dev/null
cp -a "$work/secret" "$work/other"
rm "$work/other/totp-secret.key"
kypost "$id-kp" "$id-standby" "$work/other"
timeout 60 docker wait "$id-kp" >/dev/null || fail "step 6: a server with another key kept running"
docker logs "$id-kp" 2>&1 | grep -q "shared settings differ from the deployment's: totp-secret.key" ||
  { docker logs "$id-kp" | tail -5; fail "step 6: another key was not refused"; }
docker rm -f "$id-kp" >/dev/null
echo "PASS step 6: same keys start, another totp-secret.key is refused"

# Step 7: rebuild the old primary as a standby of the new one, never start it as a primary.
docker exec "$id-standby" bash -c 'echo "host replication all all trust" >> "$PGDATA/pg_hba.conf"'
sql "$id-standby" "SELECT pg_reload_conf()" >/dev/null
standby "$id-rebuilt" "$id-standby"
echo "PASS step 7: old primary rebuilt as a standby of the new primary"
