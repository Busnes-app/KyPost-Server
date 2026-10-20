#!/usr/bin/env bash
# Disposable real-Docker regression for KyPost's network-only bootstrap.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
owned="kyq-net-owner-$$-$RANDOM"
foreign="kyq-net-foreign-$$-$RANDOM"
ids=()
cleanup() { for id in "${ids[@]}"; do docker network rm "$id" >/dev/null 2>&1 || true; done; }
trap cleanup EXIT

# Let Docker choose unused subnets, rather than colliding with a real suite LAN.
probe=$(docker network create "$owned")
ids+=("$probe")
subnet=$(docker network inspect "$probe" --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}')
docker network rm "$probe" >/dev/null
bash "$root/scripts/ensure-network.sh" "$owned" "$subnet"
first=$(docker network inspect "$owned" --format '{{.Id}}')
ids+=("$first")
bash "$root/scripts/ensure-network.sh" "$owned" "$subnet"
test "$first" = "$(docker network inspect "$owned" --format '{{.Id}}')"

other=$(docker network create "$foreign")
ids+=("$other")
subnet=$(docker network inspect "$other" --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}')
if bash "$root/scripts/ensure-network.sh" "$foreign" "$subnet"; then
  echo 'Foreign network was adopted' >&2
  exit 1
fi
test "$other" = "$(docker network inspect "$foreign" --format '{{.Id}}')"
echo 'PASS: network-only bootstrap, stable rerun and foreign-network preservation'
