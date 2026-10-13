#!/bin/sh
# Fails if a variable .env.example offers (VAR= or #VAR=) never reaches
# docker-compose.yml: setting it in .env would silently do nothing.
set -eu
cd "$(dirname "$0")/.."

# Not passed to the container on purpose; each names where it is consumed.
allow='OLLAMA_MODELS_HOST_DIR' # bind-mount source in docker-compose.yml volumes
allow="$allow KYPOST_DNS"      # docker-compose.lan-dns.yml only
allow="$allow KYPOST_MADDY_BINARY KYPOST_RECEIVING_TLS_DIR KYPOST_RECEIVING_HOSTNAME KYPOST_SMTP_BIND KYPOST_SMTP_PORT" # docker-compose.receiving.yml only

compose=$(grep -vE '^[[:space:]]*#' docker-compose.yml)
missing=$(sed -nE 's/^# ?([A-Z][A-Z0-9_]*)=.*/\1/p; s/^([A-Z][A-Z0-9_]*)=.*/\1/p' .env.example | sort -u |
  while read -r v; do
    case " $allow " in *" $v "*) continue ;; esac
    printf '%s\n' "$compose" | grep -qE "^ +${v}:|\\\$\\{${v}[:?}-]" || echo "$v"
  done)

[ -z "$missing" ] && exit 0
for v in $missing; do
  echo "::error::.env.example offers $v but docker-compose.yml never passes it" >&2
done
exit 1
