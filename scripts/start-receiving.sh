#!/bin/sh
set -eu
umask 077

refuse() { echo "native receiver refused: $1" >&2; exit 1; }
[ "$(id -u)" != 0 ] || refuse 'run as the KyPost unprivileged user'
[ "$(uname -s):$(uname -m)" = 'Linux:x86_64' ] || refuse 'only the qualified Linux x86_64 engine is supported'
[ "${KYPOST_NATIVE_RECEIVER:-}" = true ] && [ "${KYPOST_NATIVE_MAIL:-}" = true ] && [ "${KYPOST_NATIVE_RECEIVING:-}" = true ] \
    || refuse 'enable all three native receiver flags after domain setup and receiving init'

binary=${KYPOST_RECEIVER_BINARY:-/opt/kypost/receiving/maddy}
[ -f "$binary" ] && [ ! -L "$binary" ] && [ -x "$binary" ] && [ ! -w "$binary" ] \
    || refuse 'mount the qualified executable read-only as a regular nonsymlink file'
# Hash and execute the same open file; trusted host writes to that inode remain
# operator authority. A read-only mount is required in the container profile.
exec 9< "$binary"
[ "$(sha256sum /proc/self/fd/9 | cut -d ' ' -f 1)" = 6ea4b951f15b91fd81d98957e4d4bad7a0cec6d6e1d66b011c765cc9a14e05db ] \
    || refuse 'engine hash differs from the qualified Maddy 0.9.5 binary'

# Configuration validation enforces existing private roots and live authority.
# Retain the previous complete configuration if preflight fails.
case "${CONFIG_DIR:-}" in /*) ;; *) refuse 'CONFIG_DIR must be absolute' ;; esac
receiver_config_tmp=$(mktemp "$CONFIG_DIR/receiving-config.XXXXXX")
trap 'rm -f "$receiver_config_tmp"' EXIT
trap 'exit 1' HUP INT TERM
kypost-server receiving config "${KYPOST_RECEIVING_LISTEN:-0.0.0.0:2525}" \
    "${KYPOST_RECEIVING_HOSTNAME:-}" \
    "${KYPOST_RECEIVING_CERT:-/opt/kypost/receiving/tls/fullchain.pem}" \
    "${KYPOST_RECEIVING_KEY:-/opt/kypost/receiving/tls/privkey.pem}" > "$receiver_config_tmp"
mv "$receiver_config_tmp" "$CONFIG_DIR/receiving.conf"
trap - EXIT HUP INT TERM
ulimit -n 256
exec /proc/self/fd/9 --config "$CONFIG_DIR/receiving.conf" run
