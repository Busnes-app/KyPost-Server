#!/usr/bin/env bash
# Deployed cutover check for the replica profile (docs/RUNNING.md, "Replicas on one host"),
# run with a built image: a SQLite install is seeded and stopped, moved to PostgreSQL with the
# documented commands (--check, then the copy), brought up as two replicas behind HAProxy,
# signed in through the proxy with the session proven on both replicas, the client address
# checked behind an edge proxy and against a forged header, one replica stopped, backed up and
# drilled, restarted as an upgrade would (timed), restored from that capsule into a new empty
# database, then a container with a fresh config volume is refused and a changed shared
# setting is named in `docker compose logs`. Starts only its own Compose project, network and
# volumes, removes them on exit, sets no relay URL, turns AI sorting off and contacts no
# outside host. testdata/cutover-recovery-test-shares.txt is a throwaway test kit for
# testdata/cutover-recovery-public.b64, never a real one.
#
# Usage: scripts/check-replica-cutover.sh <image>
set -euo pipefail
image=${1:?usage: scripts/check-replica-cutover.sh <image>}
cd "$(dirname "$0")/.."
root=$PWD
project="kypcutover$$"
work=$(mktemp -d)
client="python3 $root/scripts/replica-cutover-client.py"

# Removes only what carries this run's project name, whichever step it stopped at.
cleanup() {
  docker rm -f "$project-intruder" "$project-edge" >/dev/null 2>&1 || true
  docker ps -aq --filter "label=com.docker.compose.project=$project" | xargs -r docker rm -f >/dev/null 2>&1 || true
  docker volume ls -q --filter "label=com.docker.compose.project=$project" | xargs -r docker volume rm >/dev/null 2>&1 || true
  docker volume rm "${project}_fresh_config" "${project}_restore" >/dev/null 2>&1 || true
  docker network ls -q --filter "label=com.docker.compose.project=$project" | xargs -r docker network rm >/dev/null 2>&1 || true
  # The single container's model mount is created by Docker; clear it as root.
  docker run --rm --user 0 -v "$work:/w" --entrypoint sh "$image" -c 'rm -rf /w/models' >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

# A fixed loopback port for the whole run, chosen once: restarts keep it.
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')
net=$((100 + $$ % 100))
edge_ip="10.199.${net}.9"
mkdir -p "$work/models"
new_pw="cutover-second-$$-password"
pg_pw=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')

cat >"$work/env" <<EOF
KYPOST_BIND=127.0.0.1
KYPOST_NETWORK=${project}-net
KYPOST_SUBNET=10.199.${net}.0/24
KYPOST_IP=10.199.${net}.5
KYPOST_REPLICA_SUBNET=172.31.${net}.0/24
KYPOST_PROXY_IP=172.31.${net}.2
KYPOST_REPLICAS=2
KYPOST_POSTGRES_PASSWORD=${pg_pw}
KYPOST_DISABLE_AI_SORTING=true
CAPTCHA_PROVIDER=none
PUSH_RELAY_URL=
APNS_RELAY_URL=
KYPOST_BACKUP_DIR=/kypost/state/backups
OLLAMA_MODELS_HOST_DIR=$work/models
TRUSTED_PROXY_HEADER=cf-connecting-ip
KYPOST_EDGE_TRUSTED_PROXY=$edge_ip
EOF
chmod 600 "$work/env"
# The test changes only the image, the published port and the shared Ollama (AI sorting is off).
cat >"$work/test.yml" <<EOF
services:
  kypost-server:
    image: ${image}
    build: !reset null
    container_name: ${project}-single
    ports: !override
      - "127.0.0.1:${port}:5866"
EOF
cat >"$work/test-replicas.yml" <<EOF
services:
  kypost-server:
    ports: !reset []
    depends_on: !override
      postgres:
        condition: service_healthy
  proxy:
    ports: !override
      - "127.0.0.1:${port}:5866"
  ollama:
    profiles: [never]
EOF
single=(docker compose -p "$project" --env-file "$work/env" -f docker-compose.yml -f "$work/test.yml")
replicas=(docker compose -p "$project" --env-file "$work/env" -f docker-compose.yml -f "$work/test.yml" -f docker-compose.replicas.yml -f "$work/test-replicas.yml")
base="http://127.0.0.1:${port}"

wait_ready() {
  for _ in $(seq 1 120); do
    if curl -fsS --max-time 2 "$base/readyz" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "not ready through $base" >&2
  return 1
}

echo "== 1. SQLite single container: seed, then a clean stop"
"${single[@]}" up -d kypost-server >/dev/null
wait_ready
admin_pw=$("${single[@]}" exec -T kypost-server sed -n 's/^password: //p' /kypost/config/first-run-password.txt)
test -n "$admin_pw"
CUTOVER_PASSWORD="$admin_pw" $client login "$base" "$work/jar" admin
CUTOVER_OLD_PASSWORD="$admin_pw" CUTOVER_PASSWORD="$new_pw" $client change-password "$base" "$work/jar" admin
CUTOVER_PASSWORD="$new_pw" $client pin-key "$base" "$work/jar" admin testdata/cutover-recovery-public.b64
key_id=$($client key-id "$base" "$work/jar")
test -n "$key_id"
"${single[@]}" stop kypost-server >/dev/null
container=$("${single[@]}" ps -aq kypost-server)
test "$(docker inspect -f '{{.State.ExitCode}}' "$container")" = 0
"${single[@]}" run --rm --no-deps --entrypoint sh kypost-server -c 'test ! -s /kypost/state/state.db-wal'
digest() { "${single[@]}" run --rm --no-deps --entrypoint sh kypost-server -c 'sha256sum /kypost/state/state.db' | cut -d' ' -f1; }
before=$(digest)
"${single[@]}" rm -f kypost-server >/dev/null

echo "== 2. The documented migration: --check, then the copy; a repeat is refused"
echo "KYPOST_DATABASE_URL=postgres://kypost:${pg_pw}@postgres:5432/kypost?sslmode=disable" >>"$work/env"
"${replicas[@]}" up -d --wait postgres >/dev/null
"${replicas[@]}" run --rm --no-deps --user kypost kypost-server /usr/local/bin/kypost-server migrate-to-postgres --check -from /kypost/state >"$work/check.out"
grep -q 'check passed' "$work/check.out"
"${replicas[@]}" run --rm --no-deps --user kypost kypost-server /usr/local/bin/kypost-server migrate-to-postgres -from /kypost/state >"$work/copy.out"
grep -q 'content digest matched' "$work/copy.out"
test "$(digest)" = "$before"
if "${replicas[@]}" run --rm --no-deps --user kypost kypost-server /usr/local/bin/kypost-server migrate-to-postgres -from /kypost/state >/dev/null 2>&1; then
  echo "a second migration was accepted" >&2
  exit 1
fi

echo "== 3. Two replicas behind HAProxy"
"${replicas[@]}" up -d --wait kypost-server proxy >/dev/null
wait_ready
mapfile -t apps < <("${replicas[@]}" ps -q kypost-server)
test "${#apps[@]}" = 2

echo "== 4. One sign-in through HAProxy, proven on both replicas"
rm -f "$work/jar"
CUTOVER_PASSWORD="$new_pw" $client login "$base" "$work/jar" admin
test "$($client key-id "$base" "$work/jar")" = "$key_id"
cookie=$($client session-cookie "$base" "$work/jar")
test -n "$cookie"
for app in "${apps[@]}"; do
  docker exec "$app" curl -fsS --max-time 10 -H "Cookie: kypost_session=${cookie}" http://127.0.0.1:5866/api/auth/me | grep -q '"authenticated":true'
  docker exec "$app" curl -fsS --max-time 10 -H "Cookie: kypost_session=${cookie}" http://127.0.0.1:5866/api/admin/backup/status | grep -q "\"keyId\":\"${key_id}\""
done

# The client address: a direct caller's forged CF-Connecting-IP is dropped by HAProxy; the
# edge proxy's is used, and the edge reaches HAProxy by the single container's name.
client_ip() { python3 -c 'import json, sys; print(json.load(sys.stdin)["clientIp"])'; }
forged=$(curl -fsS --max-time 10 -H "Cookie: kypost_session=${cookie}" -H 'CF-Connecting-IP: 203.0.113.9' "$base/api/status" | client_ip)
if [ "$forged" = 203.0.113.9 ]; then
  echo "a direct caller chose its own client address with CF-Connecting-IP" >&2
  exit 1
fi
edged=$(docker run --rm --name "$project-edge" --network "${project}-net" --ip "$edge_ip" --entrypoint curl "$image" \
  -fsS --max-time 10 -H "Cookie: kypost_session=${cookie}" -H 'CF-Connecting-IP: 198.51.100.4' http://KyPost-Server:5866/api/status | client_ip)
test "$edged" = 198.51.100.4

echo "== 5. One replica stops; the session still reads through the other"
docker stop -t 120 "${apps[0]}" >/dev/null
test "$(docker inspect -f '{{.State.ExitCode}}' "${apps[0]}")" = 0
for _ in $(seq 1 30); do
  if $client signed-in "$base" "$work/jar" 2>/dev/null; then
    break
  fi
  sleep 1
done
$client signed-in "$base" "$work/jar"

echo "== 6. Back up now, then the drill"
CUTOVER_PASSWORD="$new_pw" $client backup-now "$base" "$work/jar" admin
docker exec --user kypost "${apps[1]}" /usr/local/bin/kypost-server backup-drill >"$work/drill.out"

echo "== 7. An upgrade's restart: every replica stops, then starts (timed)"
began=$(date +%s.%N)
"${replicas[@]}" stop kypost-server >/dev/null
"${replicas[@]}" up -d --wait kypost-server >/dev/null
wait_ready
echo "restart outage: $(python3 -c "print(round($(date +%s.%N) - $began, 1))") s (stop, then up --wait, image already present)"
$client signed-in "$base" "$work/jar"

echo "== 8. Restore that capsule into a new, empty database (docs/RUNNING.md)"
"${replicas[@]}" stop kypost-server >/dev/null
# With no replica ready, HAProxy answers with KyPost's own page, never a sign-in prompt.
unavailable=$(curl -sS --max-time 10 -D - "$base/" || true)
grep -q '^HTTP/1.1 503' <<<"$unavailable" && grep -qi '^retry-after: 5' <<<"$unavailable" && grep -q 'You have not been signed out' <<<"$unavailable"
docker volume create "${project}_restore" >/dev/null
# shellcheck disable=SC2016 # expanded by the container's shell
"${replicas[@]}" run --rm --no-deps --user 0 -v "${project}_restore:/restore" --entrypoint sh kypost-server \
  -c 'cp "$(ls -t /kypost/state/backups/*.kycap | head -n 1)" /restore/backup.kycap && chown -R kypost:kypost /restore'
"${replicas[@]}" exec -T postgres createdb -U kypost kypost_restored
sed -i "s|^KYPOST_DATABASE_URL=.*|KYPOST_DATABASE_URL=postgres://kypost:${pg_pw}@postgres:5432/kypost_restored?sslmode=disable|" "$work/env"
"${replicas[@]}" run --rm --no-deps -T --user kypost -v "${project}_restore:/restore" kypost-server \
  /usr/local/bin/kypost-server restore /restore/backup.kycap /restore/recovered <testdata/cutover-recovery-test-shares.txt >"$work/restore.out"
# shellcheck disable=SC2016 # expanded by the container's shell
"${replicas[@]}" run --rm --no-deps --user kypost -v "${project}_restore:/restore" --entrypoint sh kypost-server \
  -c 'for r in config private state; do find "/kypost/$r" -mindepth 1 -delete && cp -pR "/restore/recovered/$r/." "/kypost/$r/"; done'
"${replicas[@]}" up -d --wait kypost-server proxy >/dev/null
wait_ready
rm -f "$work/jar"
CUTOVER_PASSWORD="$new_pw" $client login "$base" "$work/jar" admin
test "$($client key-id "$base" "$work/jar")" = "$key_id"

echo "== 9. A container with a fresh config volume is refused"
docker volume create "${project}_fresh_config" >/dev/null
if docker run --name "$project-intruder" --user kypost --network "${project}_database" \
  -e KYPOST_DATABASE_URL="postgres://kypost:${pg_pw}@postgres:5432/kypost?sslmode=disable" \
  -e KYPOST_BIND=127.0.0.1 -e KYPOST_DISABLE_AI_SORTING=true -e PUSH_RELAY_URL= -e APNS_RELAY_URL= \
  -v "${project}_fresh_config:/kypost/config" -v "${project}_kypost_private:/kypost/private" -v "${project}_kypost_state:/kypost/state" \
  --entrypoint /usr/local/bin/kypost-server "$image" --mode server >"$work/intruder.out" 2>&1; then
  echo "a replica with a fresh config volume started" >&2
  exit 1
fi
grep -q "does not mount the deployment's shared volumes" "$work/intruder.out"

echo "== 10. A changed shared setting is refused, and docker compose logs says which"
echo "SERVER_BASE_URL=https://changed.example.test" >>"$work/env"
"${replicas[@]}" up -d --no-deps kypost-server >/dev/null 2>&1 || true
# Captured first: under pipefail, grep -q stopping early fails the logs command (SIGPIPE).
logs=''
for _ in $(seq 1 60); do
  logs=$("${replicas[@]}" logs kypost-server 2>&1 || true)
  if grep -q 'did not start' <<<"$logs" && grep -q SERVER_BASE_URL <<<"$logs"; then
    break
  fi
  sleep 1
done
if ! grep -q 'kypost api did not start' <<<"$logs" || ! grep -q SERVER_BASE_URL <<<"$logs"; then
  tail -n 40 <<<"$logs" >&2
  echo "a refused start did not name SERVER_BASE_URL in docker compose logs" >&2
  exit 1
fi

echo "PASS replica cutover: SQLite -> PostgreSQL -> two replicas, session on both, client address, stop, backup, drill, restart, restore, intruder refused, refusal in logs"
