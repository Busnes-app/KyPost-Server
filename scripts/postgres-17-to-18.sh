#!/bin/bash
# Moves a PostgreSQL 17 data directory to 18 by dump and restore. The 17 data is only read
# (starting 17 may replay its WAL); it is never deleted or reinitialised.
#   dump     in the postgres:17 image. OLD = the 17 data directory, NEW = the directory that the
#            18 container mounts at /var/lib/postgresql. Dumps OLD once into NEW/upgrade-from-17.
#   restore  in the postgres:18 image, as its entrypoint: restores that dump once into
#            $PGDATA.part, renames it to $PGDATA, then runs the image's entrypoint with the args.
# Runs as root (compose) or as the postgres user (Kubernetes runAsUser 70).
set -Eeuo pipefail
mode=${1:-}; shift || true
new=${NEW:-/var/lib/postgresql}
dump_dir=$new/upgrade-from-17
as_pg() { if [ "$(id -u)" = 0 ]; then gosu postgres "$@"; else "$@"; fi; }
sock=$(mktemp -d)
[ "$(id -u)" != 0 ] || chown postgres "$sock"

case $mode in
dump)
  old=${OLD:?set OLD to the PostgreSQL 17 data directory}
  checkpoint() { LC_ALL=C as_pg pg_controldata "$old" | awk -F': *' '/^Latest checkpoint location/ { print $2 }'; }
  # PostgreSQL's own lock check cannot see a server in another container (its PID 1 looks
  # like our grandparent), so a 17 still running on OLD would get a second postmaster.
  refuse_running() {
    echo "postgres-17-to-18: $old/postmaster.pid exists: PostgreSQL 17 is running on it, or did not stop cleanly; stop it (start it once with the previous setup and stop it, if it crashed), then start again" >&2
    exit 1
  }
  if [ -s "$new/18/docker/PG_VERSION" ] || [ -s "$dump_dir/dump.sql" ]; then
    if [ ! -s "$old/PG_VERSION" ]; then echo "postgres-17-to-18: already done"; exit 0; fi
    [ ! -e "$old/postmaster.pid" ] || refuse_running
    # Kit v2 kept no checkpoint: take the current one as the moved state.
    [ -s "$dump_dir/source-checkpoint" ] || checkpoint > "$dump_dir/source-checkpoint"
    # Every start of 17 writes a new checkpoint. A changed one means 17 ran after the move (a
    # rollback), so 18 lacks what 17 wrote since: never start 18 on it silently.
    if [ "$(checkpoint)" = "$(cat "$dump_dir/source-checkpoint")" ]; then echo "postgres-17-to-18: already done"; exit 0; fi
    echo "postgres-17-to-18: PostgreSQL 17 has run on $old since it was moved to 18, so 18 lacks what 17 wrote since then. To move 17 again: stop everything, rename $new/18 and $dump_dir with a suffix such as .replaced (nothing is deleted), then start again. To keep 18 as it is: delete $dump_dir/source-checkpoint, then start again." >&2
    exit 1
  fi
  if [ ! -s "$old/PG_VERSION" ]; then
    echo "postgres-17-to-18: no PostgreSQL 17 data in $old, nothing to move"; exit 0
  fi
  if [ "$(cat "$old/PG_VERSION")" != 17 ]; then
    echo "postgres-17-to-18: $old holds PostgreSQL $(cat "$old/PG_VERSION"), expected 17" >&2; exit 1
  fi
  [ ! -e "$old/postmaster.pid" ] || refuse_running
  # The dump and the restored cluster each take at most about the size of the 17 data.
  need=$(( $(du -sk "$old" | cut -f1) * 2 ))
  free=$(df -Pk "$new" | awk 'NR == 2 { print $4 }')
  if [ "$free" -lt "$need" ]; then
    echo "postgres-17-to-18: $new has $((free / 1024)) MiB free, the move needs $((need / 1024)) MiB; free space or grow the volume, then start again" >&2
    exit 1
  fi
  mkdir -p "$dump_dir"
  [ "$(id -u)" != 0 ] || chown postgres "$new" "$dump_dir"
  # No network, and a private socket that only trusts local connections.
  printf 'local all all trust\n' > "$sock/hba"
  [ "$(id -u)" != 0 ] || chown postgres "$sock/hba"
  as_pg pg_ctl -D "$old" -w -t 3600 -l "$sock/log" \
    -o "-c listen_addresses= -c unix_socket_directories=$sock -c hba_file=$sock/hba" start ||
    { cat "$sock/log" >&2; exit 1; }
  trap 'as_pg pg_ctl -D "$old" -m fast -w stop' EXIT
  su=$(as_pg psql -h "$sock" -U "${POSTGRES_USER:-postgres}" -d template1 -XAtc "select rolname from pg_roles where oid = 10")
  printf '%s\n' "$su" > "$dump_dir/superuser"
  as_pg pg_dumpall -h "$sock" -U "$su" -f "$dump_dir/dump.sql.part"
  as_pg sync "$dump_dir/dump.sql.part"
  trap - EXIT
  as_pg pg_ctl -D "$old" -m fast -w stop
  # 17 as the dump left it; a later start of 17 (a rollback) changes this.
  checkpoint > "$dump_dir/source-checkpoint"
  mv "$dump_dir/dump.sql.part" "$dump_dir/dump.sql"
  sync "$dump_dir"
  echo "postgres-17-to-18: dumped $old into $dump_dir/dump.sql; the 17 data is kept"
  ;;
restore)
  final=${PGDATA:?}
  if [ ! -s "$final/PG_VERSION" ] && [ -s "$dump_dir/dump.sql" ]; then
    # shellcheck source=/dev/null # the image's entrypoint, only inside the postgres image
    source /usr/local/bin/docker-entrypoint.sh
    # initdb's superuser is the 17 cluster's bootstrap role, so the dump's roles map one to one.
    POSTGRES_USER=$(cat "$dump_dir/superuser")
    export POSTGRES_USER PGDATA=$final.part
    rm -rf "$PGDATA" # a restore an earlier start did not finish; never the 17 data
    docker_setup_env
    docker_create_db_directories
    as_pg bash -c 'source /usr/local/bin/docker-entrypoint.sh; docker_setup_env; docker_init_database_dir; pg_setup_hba_conf'
    as_pg pg_ctl -D "$PGDATA" -w -o "-c listen_addresses= -c unix_socket_directories=$sock" start
    # pg_dumpall writes CREATE ROLE for the role it ran as, which initdb already made: drop that
    # one line, only in the roles section above the first \connect (row data comes later).
    q=$(as_pg psql -h "$sock" -U "$POSTGRES_USER" -d postgres -XAtc "select quote_ident(current_user)")
    awk -v skip="CREATE ROLE $q;" 'seen || $0 != skip { print } /^\\connect / { seen = 1 }' "$dump_dir/dump.sql" |
      as_pg psql -h "$sock" -U "$POSTGRES_USER" -d postgres -X -q -v ON_ERROR_STOP=1
    as_pg vacuumdb -h "$sock" -U "$POSTGRES_USER" --all --analyze-only -q
    as_pg pg_ctl -D "$PGDATA" -m fast -w stop
    sync "$PGDATA"
    mv "$PGDATA" "$final"
    sync "$(dirname "$final")"
    export PGDATA=$final
    echo "postgres-17-to-18: restored $dump_dir/dump.sql into $final"
  fi
  rm -rf "$sock"
  exec docker-entrypoint.sh "$@"
  ;;
*) echo "usage: postgres-17-to-18.sh dump | restore postgres [args...]" >&2; exit 2 ;;
esac
