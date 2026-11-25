#!/usr/bin/env bash
# Proves the replica overlay's PostgreSQL 17 -> 18 move (docs/RUNNING.md, "PostgreSQL 18") on
# real data: a dump beside a running 17 refuses and writes nothing; the next `up -d` over a
# running 17 stack, seeded with the previous compose files, comes up on 18 with its rows, roles,
# passwords (a wrong one refused) and C locale; the 17 volume still starts 17 (rollback); a
# second `up` is a no-op; a dump that fails to restore never yields a cluster; the documented
# clean-up leaves 18 running; a fresh install initialises. Starts only the database services
# of its own Compose projects and removes them, their volumes and networks on exit.
#
# Usage: scripts/test-postgres-17-to-18.sh [commit whose compose files run PostgreSQL 17]
set -Eeuo pipefail
cd "$(dirname "$0")/.."
root=$PWD
prev=${1:-cf5c3c801b0b6f9769907fafb5b30b840b2352d2}
project="kyppg18$$"
fresh="${project}f"
work=$(mktemp -d)
P18=postgres:18.7-alpine3.24@sha256:0115267ab5a509022df9319c77be365cef335adb71556c75f60f5360ee247e71

cleanup() {
  for p in "$project" "$fresh"; do
    docker ps -aq --filter "label=com.docker.compose.project=$p" | xargs -r docker rm -f >/dev/null 2>&1 || true
    docker volume ls -q --filter "label=com.docker.compose.project=$p" | xargs -r docker volume rm >/dev/null 2>&1 || true
    docker network ls -q --filter "label=com.docker.compose.project=$p" | xargs -r docker network rm >/dev/null 2>&1 || true
  done
  rm -rf "$work"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; "${new[@]}" logs postgres-17-dump postgres 2>&1 | tail -40 >&2 || true; exit 1; }

git cat-file -e "$prev:docker-compose.replicas.yml" 2>/dev/null || git fetch -q --no-tags --depth=1 origin "$prev"
git show "$prev:docker-compose.yml" >"$work/old-base.yml"
git show "$prev:docker-compose.replicas.yml" >"$work/old-replicas.yml"
grep -q 'image: postgres:17' "$work/old-replicas.yml" || { echo "$prev does not run PostgreSQL 17" >&2; exit 1; }

net=$((100 + $$ % 100))
pg_pw=$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
cat >"$work/env" <<EOF
KYPOST_BIND=127.0.0.1
KYPOST_NETWORK=${project}-net
KYPOST_SUBNET=10.197.${net}.0/24
KYPOST_IP=10.197.${net}.5
KYPOST_REPLICA_SUBNET=10.196.${net}.0/24
KYPOST_PROXY_IP=10.196.${net}.2
KYPOST_POSTGRES_PASSWORD=${pg_pw}
KYPOST_DATABASE_URL=postgres://kypost:${pg_pw}@postgres:5432/kypost?sslmode=disable
EOF
chmod 600 "$work/env"
compose=(docker compose --project-directory "$root" --env-file "$work/env")
old=("${compose[@]}" -p "$project" -f "$work/old-base.yml" -f "$work/old-replicas.yml")
new=("${compose[@]}" -p "$project" -f docker-compose.yml -f docker-compose.replicas.yml)
up() { "$@" up -d --wait --wait-timeout 600 postgres >/dev/null 2>"$work/up.err" || { cat "$work/up.err" >&2; return 1; }; }
down_db() { "$@" stop postgres >/dev/null 2>&1; "$@" rm -f postgres >/dev/null 2>&1; }
# From another container on the database network, as the replicas connect: inside the server
# container loopback is trust, so only this proves the password.
client() { docker run --rm --network "$1_database" -e PGPASSWORD="$2" "$P18" psql -h postgres -U "$3" -d kypost -XAtc "$4"; }
q() { client "$project" "${3:-$pg_pw}" "${2:-kypost}" "$1"; }
in_vol() { docker run --rm --network none -v "${project}_$1:/v" "$P18" sh -c "$2"; }

echo "== 1. PostgreSQL 17 data, from the previous compose files"
up "${old[@]}" || fail "17 did not start from $prev"
"${old[@]}" exec -T postgres psql -U kypost -d kypost -X -q -v ON_ERROR_STOP=1 >/dev/null <<'SQL'
create role reader login password 'readerpass';
create table t(id bigserial primary key, s text unique, j jsonb);
insert into t(s, j) select 'row' || g, jsonb_build_object('g', g) from generate_series(1, 50000) g;
create table c(x text primary key);
insert into c values ('a'), ('B'), ('é');
grant select on c to reader;
SQL

echo "== 1b. A dump beside the running 17 refuses and writes nothing"
out=$("${new[@]}" run --rm postgres-17-dump 2>&1) && fail "the dump ran beside a live 17: $out"
grep -q 'postmaster.pid exists' <<<"$out" || fail "no live-server message: $out"
in_vol kypost_postgres 'test ! -e /v/upgrade-from-17 && test ! -e /v/18' || fail "the dump beside a live 17 wrote to the 18 volume"
[ "$("${old[@]}" exec -T postgres psql -U kypost -d kypost -XAtc 'select count(*) from t')" = 50000 ] || fail "17 is not still serving"

echo "== 2. The next up -d, with the 17 stack still running (no down), moves it to 18"
up "${new[@]}" || fail "18 did not start after the move"
# Compose recreates (stops) 17 before it starts the dump, so the dump ran and found no server.
"${new[@]}" logs postgres-17-dump 2>&1 | grep -q 'dumped /var/lib/postgresql/data' || fail "the dump did not run on up"
"${new[@]}" logs postgres 2>&1 | grep -q 'restored' || fail "18 did not restore the dump"
[ "$(q 'show server_version_num' | cut -c1-2)" = 18 ] || fail "not PostgreSQL 18"
[ "$(q "select count(*) || ',' || max(id) || ',' || sum((j->>'g')::int) from t")" = "50000,50000,1250025000" ] || fail "rows"
[ "$(q "select string_agg(x, ',' order by x) from c" reader readerpass)" = "B,a,é" ] || fail "reader password or C collation"
q 'select 1' kypost wrong-password >/dev/null 2>&1 && fail "a wrong password logged in"
q 'select 1' reader wrong-password >/dev/null 2>&1 && fail "a wrong reader password logged in"
[ "$(q "select datcollate || ',' || pg_encoding_to_char(encoding) from pg_database where datname = 'kypost'")" = C,UTF8 ] || fail "database locale"
[ "$(q 'show data_directory')" = /var/lib/postgresql/18/docker ] || fail "data directory"
[ "$(in_vol kypost_pgdata 'cat /v/PG_VERSION')" = 17 ] || fail "the 17 volume changed"

echo "== 3. Rollback: the previous compose files start 17 on the kept volume"
down_db "${new[@]}"
up "${old[@]}" || fail "17 no longer starts on its volume"
[ "$("${old[@]}" exec -T postgres psql -U kypost -d kypost -XAtc 'select count(*) from t')" = 50000 ] || fail "17 data changed"
down_db "${old[@]}"

echo "== 4. Another up is a no-op"
up "${new[@]}" || fail "18 did not restart"
dumps=$("${new[@]}" logs postgres-17-dump 2>&1)
{ [ "$(grep -c "dumped" <<<"$dumps")" = 1 ] && grep -q "already done" <<<"$dumps"; } || fail "the second up dumped again: $dumps"
"${new[@]}" logs postgres 2>&1 | grep -q 'restored' && fail "the second up restored again"
[ "$(q 'select count(*) from t')" = 50000 ] || fail "rows after the second up"

echo "== 5. A dump that fails to restore never yields a cluster"
down_db "${new[@]}"
in_vol kypost_postgres 'mv /v/18/docker /v/18/docker.saved && cp /v/upgrade-from-17/dump.sql /v/dump.good && echo "THIS IS NOT SQL;" >> /v/upgrade-from-17/dump.sql'
"${new[@]}" up -d postgres >/dev/null 2>&1
id=$("${new[@]}" ps -aq postgres)
for _ in $(seq 1 300); do
  [ "$(docker inspect -f '{{.RestartCount}}' "$id")" -ge 2 ] && break
  sleep 1
done
[ "$(docker inspect -f '{{.RestartCount}}' "$id")" -ge 2 ] || fail "a bad dump did not fail twice"
[ "$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$id")" != healthy ] || fail "a bad dump became healthy"
in_vol kypost_postgres 'test ! -e /v/18/docker' || fail "a bad dump produced a cluster"
down_db "${new[@]}"
in_vol kypost_postgres 'rm -rf /v/18/docker.part && mv /v/18/docker.saved /v/18/docker && mv /v/dump.good /v/upgrade-from-17/dump.sql'

echo "== 6. The documented clean-up: the 17 volume and the dump go, 18 keeps running"
"${new[@]}" down >/dev/null 2>&1
docker volume rm "${project}_kypost_pgdata" >/dev/null
"${new[@]}" run --rm --no-deps --entrypoint rm postgres -r /var/lib/postgresql/upgrade-from-17 >/dev/null 2>&1
up "${new[@]}" || fail "18 did not start after the clean-up"
[ "$(q 'select count(*) from t')" = 50000 ] || fail "rows after the clean-up"
in_vol kypost_postgres 'test ! -e /v/upgrade-from-17' || fail "the dump is still there"
"${new[@]}" logs postgres-17-dump 2>&1 | grep -q 'already done' || fail "the dump ran again after the clean-up"

echo "== 7. A fresh install initialises 18"
fresh_new=("${compose[@]}" -p "$fresh" -f docker-compose.yml -f docker-compose.replicas.yml)
sed -i "s/^KYPOST_NETWORK=.*/KYPOST_NETWORK=${fresh}-net/; s/^KYPOST_SUBNET=10.197/KYPOST_SUBNET=10.195/; s/^KYPOST_IP=10.197/KYPOST_IP=10.195/; s/^KYPOST_REPLICA_SUBNET=10.196/KYPOST_REPLICA_SUBNET=10.194/; s/^KYPOST_PROXY_IP=10.196/KYPOST_PROXY_IP=10.194/" "$work/env"
up "${fresh_new[@]}" || fail "a fresh 18 did not start"
"${fresh_new[@]}" logs postgres-17-dump 2>&1 | grep -q 'nothing to move' || fail "a fresh install did not skip the dump"
"${fresh_new[@]}" logs postgres 2>&1 | grep -q 'restored' && fail "a fresh install restored something"
[ "$(client "$fresh" "$pg_pw" kypost "select current_setting('data_directory') || ',' || datcollate from pg_database where datname = 'kypost'")" = /var/lib/postgresql/18/docker,C ] ||
  fail "fresh data directory or locale"
client "$fresh" wrong-password kypost 'select 1' >/dev/null 2>&1 && fail "a fresh install let a wrong password in"

echo "PASS postgres 17 -> 18: dump beside a live 17 refused, move on up -d over a running 17, rows, roles, passwords, C locale, rollback to 17, rerun, bad dump, clean-up, fresh install"
