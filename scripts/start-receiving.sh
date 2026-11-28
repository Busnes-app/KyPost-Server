#!/bin/sh
set -eu
umask 077

refuse() { echo "native receiver refused: $1" >&2; exit 1; }
[ "$(id -u)" != 0 ] || refuse 'run as the KyPost unprivileged user'
[ "$(uname -s):$(uname -m)" = 'Linux:x86_64' ] || refuse 'only the qualified Linux x86_64 engine is supported'
# An MX host of the hosted profile (docker-compose.mx.yml) holds no state: it forwards
# every check to the replicas, so it needs the receiver flag and multi mode only.
if [ -n "${KYPOST_RECEIVING_REMOTE:-}" ]; then
    { [ "${KYPOST_NATIVE_RECEIVER:-}" = true ] && [ "${KYPOST_TENANCY:-}" = multi ]; } \
        || refuse 'an MX host needs KYPOST_NATIVE_RECEIVER=true and KYPOST_TENANCY=multi'
else
    { [ "${KYPOST_NATIVE_RECEIVER:-}" = true ] && [ "${KYPOST_NATIVE_MAIL:-}" = true ] && [ "${KYPOST_NATIVE_RECEIVING:-}" = true ]; } \
        || refuse 'enable all three native receiver flags after domain setup and receiving init'
fi

binary=${KYPOST_RECEIVER_BINARY:-/opt/kypost/receiving/maddy}
{ [ -f "$binary" ] && [ ! -L "$binary" ] && [ -x "$binary" ] && [ ! -w "$binary" ]; } \
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
render() {
    kypost-server receiving config "${KYPOST_RECEIVING_LISTEN:-0.0.0.0:2525}" \
        "${KYPOST_RECEIVING_HOSTNAME:-}" \
        "${KYPOST_RECEIVING_CERT:-/opt/kypost/receiving/tls/fullchain.pem}" \
        "${KYPOST_RECEIVING_KEY:-/opt/kypost/receiving/tls/privkey.pem}" > "$receiver_config_tmp"
}
render
mv "$receiver_config_tmp" "$CONFIG_DIR/receiving.conf"
trap - EXIT HUP INT TERM
# shellcheck disable=SC3045 # the image's /bin/sh is dash, which supports -n
ulimit -n 256
# The pinned binary's DefaultGODEBUG turns Go's X25519MLKEM768 hybrid off on STARTTLS (its
# go.mod predates Go 1.24). Turn it back on; classical senders still connect.
# Append, so an operator GODEBUG (the strong-crypto overlay's fips140=on) still reaches the helpers.
export GODEBUG="${GODEBUG:+$GODEBUG,}tlsmlkem=1" # pqc-tls:allow: enables the hybrid, never disables it
[ -n "${KYPOST_RECEIVING_REMOTE:-}" ] || exec /proc/self/fd/9 --config "$CONFIG_DIR/receiving.conf" run

# MX host: re-fetch the replicas' domains every 5 minutes (60 ticks of 5 s) and restart
# Maddy only when the profile changed. A failed fetch keeps the running profile; Maddy
# exiting exits too, so the container restarts.
maddy=
stop() { [ -z "$maddy" ] || { kill "$maddy" 2>/dev/null; wait "$maddy"; }; exit 0; }
trap stop HUP INT TERM
while :; do
    /proc/self/fd/9 --config "$CONFIG_DIR/receiving.conf" run &
    maddy=$!
    ticks=0
    while :; do
        sleep 5 &
        wait $! || true
        kill -0 "$maddy" 2>/dev/null || { wait "$maddy" || true; refuse 'Maddy exited'; }
        ticks=$((ticks + 1))
        [ "$ticks" -ge 60 ] || continue
        ticks=0
        receiver_config_tmp=$(mktemp "$CONFIG_DIR/receiving-config.XXXXXX")
        if render && ! cmp -s "$receiver_config_tmp" "$CONFIG_DIR/receiving.conf"; then
            mv "$receiver_config_tmp" "$CONFIG_DIR/receiving.conf"
            kill "$maddy"
            wait "$maddy" || true
            break
        fi
        rm -f "$receiver_config_tmp"
    done
done
