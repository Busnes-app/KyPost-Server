#!/usr/bin/env bash
# Small host-updater self-check: no Docker daemon, registry, or systemd needed.
set -euo pipefail

root_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT
mkdir -p "$tmp_dir/bin"

cat >"$tmp_dir/bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${FAKE_DOCKER_STATE:?}"
printf '%s\n' "$*" >>"$state.calls"
if [[ "$1 ${2:-}" == "compose version" || "$1 ${2:-}" == "buildx version" ]]; then
  echo "fake"
elif [[ "$1 ${2:-} ${3:-}" == "compose ps -aq" ]]; then
  echo "container-1"
elif [[ "$1" == "inspect" && "$*" == *"config_files"* ]]; then
  # The overlays the running container was created with.
  # ...including a previous run's image override, which that run deleted.
  echo "/srv/kypost/docker-compose.yml,/srv/kypost/docker-compose.receiving.yml,/srv/kypost/.kypost-update.AbC123.yml"
elif [[ "$1" == "compose" && " $* " == *" config --images kypost-server "* ]]; then
  echo "ghcr.io/busnes-app/kypost-server:stable"
elif [[ "$1" == "compose" && " $* " == *" images -q kypost-server "* ]]; then
  # Compose prints the bare hex; image inspect prefixes sha256:.
  echo "previous"
elif [[ "$1" == "tag" ]]; then
  :
elif [[ "$1 ${2:-}" == "buildx imagetools" ]]; then
  echo "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
elif [[ "$1" == "pull" ]]; then
  :
elif [[ "$1 ${2:-}" == "image inspect" && "$*" == *"RepoDigests"* ]]; then
  echo "ghcr.io/busnes-app/kypost-server@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
elif [[ "$1 ${2:-}" == "image inspect" ]]; then
  if [[ "${FAKE_DOCKER_MODE:-}" == "noop" ]]; then echo "sha256:previous"; else echo "sha256:candidate"; fi
elif [[ "$1" == "compose" && " $* " == *" up "* ]]; then
  count=0
  [[ -f "$state" ]] && count="$(cat "$state")"
  count=$((count + 1))
  printf '%s' "$count" >"$state"
  if [[ "${FAKE_DOCKER_MODE:-}" == "rollback" && "$count" == 1 ]]; then exit 1; fi
else
  echo "unexpected docker invocation: $*" >&2
  exit 1
fi
EOF
chmod +x "$tmp_dir/bin/docker"

cat >"$tmp_dir/bin/gh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
[[ "$1 ${2:-}" == "attestation verify" ]] || exit 1
EOF
chmod +x "$tmp_dir/bin/gh"

run_case() {
  local mode="$1" output status
  : >"$tmp_dir/$mode-count"
  set +e
  output="$(FAKE_DOCKER_MODE="$mode" FAKE_DOCKER_STATE="$tmp_dir/$mode-count" KYPOST_COMPOSE_DIR="$tmp_dir" PATH="$tmp_dir/bin:$PATH" "$root_dir/scripts/update-host.sh" 2>&1)"
  status=$?
  set -e
  case "$mode" in
    noop)
      [[ "$status" == 0 ]] || { echo "no-op updater exit = $status" >&2; exit 1; }
      grep -q 'action=verify .*result=started' <<<"$output"
      grep -q 'action=update .*result=already-current' <<<"$output"
      [[ ! -s "$tmp_dir/$mode-count" ]] || { echo "no-op started Compose" >&2; exit 1; }
      grep -qx 'tag ghcr.io/busnes-app/kypost-server@sha256:a\{64\} ghcr.io/busnes-app/kypost-server:stable' "$tmp_dir/$mode-count.calls" \
        || { echo "no-op left the local tag where an older updater put it" >&2; exit 1; }
      ;;
    rollback)
      [[ "$status" == 1 ]] || { echo "failed update exit = $status" >&2; exit 1; }
      grep -q 'action=rollback .*result=healthy' <<<"$output"
      [[ "$(cat "$tmp_dir/$mode-count")" == 2 ]] || { echo "rollback did not recreate twice" >&2; exit 1; }
      grep -qx 'tag ghcr.io/busnes-app/kypost-server@sha256:b\{64\} ghcr.io/busnes-app/kypost-server:stable' "$tmp_dir/$mode-count.calls" \
        || { echo "rollback did not move the tag back" >&2; exit 1; }
      ;;
    update)
      [[ "$status" == 0 ]] || { echo "update exit = $status" >&2; exit 1; }
      grep -q 'action=update .*result=healthy' <<<"$output"
      grep -q -- '-f /srv/kypost/docker-compose.yml -f /srv/kypost/docker-compose.receiving.yml -f .* up ' "$tmp_dir/$mode-count.calls" \
        || { echo "update dropped the running overlays" >&2; exit 1; }
      ! grep -q 'kypost-update.AbC123' "$tmp_dir/$mode-count.calls" \
        || { echo "update reused the previous run's deleted override" >&2; exit 1; }
      grep -qx 'tag ghcr.io/busnes-app/kypost-server@sha256:a\{64\} ghcr.io/busnes-app/kypost-server:stable' "$tmp_dir/$mode-count.calls" \
        || { echo "update left the local tag on the old image" >&2; exit 1; }
      ;;
  esac
}

run_case noop
run_case rollback
run_case update
echo "update-host self-check passed"
