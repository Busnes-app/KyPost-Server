#!/usr/bin/env bash
# Network-only bootstrap owned by KyPost; no server, volume or credential is created.
set -euo pipefail
network=${1:-kypost-net}
subnet=${2:-10.89.0.0/24}
if (( $# > 2 )) || [[ ! $network =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$ ]]; then
  echo 'usage: bash scripts/ensure-network.sh [NETWORK [SUBNET]]' >&2
  exit 2
fi
inspect() {
  docker network inspect "$network" --format '{{.Driver}}|{{index .Labels "com.docker.compose.project"}}|{{index .Labels "com.docker.compose.network"}}|{{range .IPAM.Config}}{{.Subnet}}{{end}}'
}
if ! have=$(inspect 2>/dev/null); then
  # Docker creates by name without replacing an existing network. Re-inspect even
  # after a racing create; ownership must agree before anyone joins it.
  docker network create --driver bridge --subnet "$subnet" \
    --label com.docker.compose.project=kypost-server \
    --label com.docker.compose.network=kypost-net "$network" >/dev/null || true
  have=$(inspect)
fi
if [[ $have != "bridge|kypost-server|kypost-net|$subnet" ]]; then
  echo 'Existing network differs or has another owner; preserved. Use the documented legacy external override, or choose another network.' >&2
  exit 1
fi
printf 'KyPost-owned network %s is ready; no application was installed.\n' "$network"
