#!/usr/bin/env bash
# Native-mail dev harness: a real KyIdentity, KyPost, dnsmasq, Maddy and the
# kypost-demo-IMAP fixture, all inside one loopback-only network namespace.
# Nothing listens on a host interface and the KyPost binary runs unmodified:
# DNS answers come from dnsmasq through a bind-mounted /etc/resolv.conf, not
# from any flag, build tag or environment variable in KyPost.
#
#   native-mail-harness.sh up        build, boot and pair everything (phase 1)
#   native-mail-harness.sh provision assign the KyIdentity user, init receiving, start Maddy (phase 2)
#   native-mail-harness.sh exec CMD  run CMD inside the namespace as the unprivileged user
#   native-mail-harness.sh send FROM TO SUBJECT [BODY]  deliver over SMTP+STARTTLS to the MX
#   native-mail-harness.sh relay-sent  list what the relay (fixture SMTP) accepted
#   native-mail-harness.sh down      stop everything and delete the harness directory
#
# See docs/DEVELOPMENT.md#native-mail-harness for inputs and the full walk-through.
set -euo pipefail
umask 077

REPO=$(cd "$(dirname "$0")/../.." && pwd)
HERE="$REPO/scripts/dev/native-mail-harness"
# On disk, not /tmp: KyPost refuses mail while its drive has under 5 GiB free.
HOST_DIR=${KPH_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/kypost-native-harness}
H=/mnt                       # HOST_DIR is bind-mounted here inside the namespace
DOMAIN=harness.test
KYPOST_ORIGIN=https://mail.$DOMAIN:8443
ISSUER=https://id.$DOMAIN
MADDY_SHA=6ea4b951f15b91fd81d98957e4d4bad7a0cec6d6e1d66b011c765cc9a14e05db

die() { echo "native-mail-harness: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || die "missing $1"; }

holder_pid() {
  local p
  p=$(cat "$HOST_DIR/ns.pid" 2>/dev/null) || die "not running (no $HOST_DIR/ns.pid); run up first"
  kill -0 "$p" 2>/dev/null || die "namespace holder $p is gone; run down, then up"
  echo "$p"
}

# Enter the namespace as its root (the host user) ...
# KPH_INSIDE marks commands already in the namespace (drivers calling send/relay-sent).
ns_root() {
  if [ -n "${KPH_INSIDE:-}" ]; then "$@"; return; fi
  KPH_INSIDE=1 nsenter -t "$(holder_pid)" -U -n -m --preserve-credentials --wd="$PWD" -- "$@"
}
# ... or as uid 1000 in a nested user namespace, which is how every service runs.
ns_user() {
  if [ -n "${KPH_INSIDE:-}" ]; then "$@"; return; fi
  ns_root unshare -U --map-user=1000 --map-group=1000 -- "$@"
}

# ---------------------------------------------------------------------------
# Inside the namespace (root of the outer user namespace)
# ---------------------------------------------------------------------------

as_user() { unshare -U --map-user=1000 --map-group=1000 -- "$@"; }

start() { # name cmd... ; runs as uid 1000, logs to $H/logs/name.log
  local name=$1; shift
  # A plain command, not a function, so $! is the service itself (unshare and env exec).
  unshare -U --map-user=1000 --map-group=1000 -- "$@" >>"$H/logs/$name.log" 2>&1 &
  echo $! >"$H/run/$name.pid"
}

wait_tcp() { # host port seconds
  local i
  for ((i = 0; i < $3 * 4; i++)); do
    (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null && return 0
    sleep 0.25
  done
  die "nothing listening on $1:$2 after $3 s"
}

dns_restart() {
  [ -f "$H/run/dnsmasq.pid" ] && kill "$(cat "$H/run/dnsmasq.pid")" 2>/dev/null || true
  sleep 0.3
  # Unprivileged (setgroups is denied in this user namespace), so it never drops privileges.
  as_user dnsmasq --conf-file="$H/dns/dnsmasq.conf" --pid-file="$H/run/dnsmasq.pid"
  wait_tcp 127.0.0.1 53 5
}

gen_tls() {
  mkdir -p "$H/tls"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 \
    -subj "/CN=KyPost native-mail harness CA" -keyout "$H/tls/ca.key" -out "$H/tls/ca.pem" \
    -addext basicConstraints=critical,CA:TRUE -addext keyUsage=critical,keyCertSign,cRLSign 2>/dev/null
  openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=mail.$DOMAIN" \
    -keyout "$H/tls/privkey.pem" -out "$H/tls/leaf.csr" 2>/dev/null
  printf 'subjectAltName=DNS:mail.%s,DNS:mx.%s,DNS:id.%s,DNS:smtp.relay.test\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature\nextendedKeyUsage=serverAuth\n' \
    "$DOMAIN" "$DOMAIN" "$DOMAIN" >"$H/tls/leaf.ext"
  openssl x509 -req -in "$H/tls/leaf.csr" -CA "$H/tls/ca.pem" -CAkey "$H/tls/ca.key" -CAcreateserial \
    -days 30 -extfile "$H/tls/leaf.ext" -out "$H/tls/leaf.pem" 2>/dev/null
  cat "$H/tls/leaf.pem" "$H/tls/ca.pem" >"$H/tls/fullchain.pem"
  # The fixture loads server.key/server.crt from its TLS_KEY_DIR instead of generating its own.
  mkdir -p "$H/fixture/keys" "$H/fixture/public"
  cp "$H/tls/privkey.pem" "$H/fixture/keys/server.key"
  cp "$H/tls/fullchain.pem" "$H/fixture/keys/server.crt"
  # Chromium trusts the CA through an NSS database under the browser's HOME.
  mkdir -p "$H/browser-home/.pki/nssdb"
  certutil -N -d "sql:$H/browser-home/.pki/nssdb" --empty-password
  certutil -A -d "sql:$H/browser-home/.pki/nssdb" -n kph-ca -t C,, -i "$H/tls/ca.pem"
}

kypost_env() {
  cat <<EOF
HOME=$H/kypost
PATH=$H/bin:/usr/bin:/bin
CONFIG_DIR=$H/kypost/config
STATE_DIR=$H/kypost/state
LOG_DIR=$H/kypost/logs
SECRET_DIR=$H/kypost/secrets
FRONTEND_DIR=$REPO/frontend/dist
WEB_PORT=8443
TLS_CERT_FILE=$H/tls/fullchain.pem
TLS_KEY_FILE=$H/tls/privkey.pem
SERVER_BASE_URL=$KYPOST_ORIGIN
SSL_CERT_FILE=$H/tls/ca.pem
PAIRING_SECRET=$(cat "$H/secrets/pairing")
KYPOST_NATIVE_MAIL=true
KYPOST_NATIVE_RECEIVING=$([ -f "$H/run/receiving-on" ] && echo true || echo false)
KYPOST_DISABLE_AI_SORTING=true
CAPTCHA_PROVIDER=none
PUSH_RELAY_URL=
APNS_RELAY_URL=
EOF
}

kypost() { local e; mapfile -t e < <(kypost_env); as_user env -i "${e[@]}" "$H/bin/kypost-server" "$@"; }

start_kypost() {
  local e; mapfile -t e < <(kypost_env)
  start kypost env -i "${e[@]}" "$H/bin/kypost-server"
  for _ in $(seq 120); do
    curl -fsS --cacert "$H/tls/ca.pem" "$KYPOST_ORIGIN/api/health" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  die "KyPost did not become healthy; see $HOST_DIR/logs/kypost.log"
}

stop_pid() { # TERM, wait up to 30 s for the exit (KyPost drains), then KILL
  local f=$H/run/$1.pid p
  [ -f "$f" ] || return 0
  p=$(cat "$f"); rm -f "$f"
  kill "$p" 2>/dev/null || return 0
  for _ in $(seq 60); do kill -0 "$p" 2>/dev/null || return 0; sleep 0.5; done
  kill -KILL "$p" 2>/dev/null || true
}

inner_up() {
  ip link set lo up
  for i in 2 3 4; do ip addr add "10.77.0.$i/32" dev lo; done
  # uid 1000 in the nested namespace binds 25 and 443 without capabilities.
  echo 0 >/proc/sys/net/ipv4/ip_unprivileged_port_start
  mount --bind "$HOST_DIR" "$H"
  printf 'nameserver 127.0.0.1\noptions attempts:1 timeout:1\n' >"$H/dns/resolv.conf"
  mount --bind "$H/dns/resolv.conf" /etc/resolv.conf
  # glibc users (curl, Node, Chromium) would otherwise ask the host's
  # systemd-resolved over its socket instead of dnsmasq.
  printf 'passwd: files\ngroup: files\nhosts: files dns\n' >"$H/dns/nsswitch.conf"
  mount --bind "$H/dns/nsswitch.conf" /etc/nsswitch.conf
  : >"$H/dns/txt.conf"
  cat >"$H/dns/dnsmasq.conf" <<EOF
no-resolv
no-hosts
no-poll
listen-address=127.0.0.1
bind-interfaces
log-queries
log-facility=$H/logs/dnsmasq.log
host-record=id.$DOMAIN,10.77.0.2
host-record=mail.$DOMAIN,10.77.0.3
host-record=mx.$DOMAIN,10.77.0.3
host-record=smtp.relay.test,10.77.0.4
mx-host=$DOMAIN,mx.$DOMAIN,10
conf-file=$H/dns/txt.conf
EOF
  dns_restart
  gen_tls

  # Fixture: IMAPS 993, STARTTLS 587, CardDAV 9443 and the implicit-TLS 465
  # listener the domain relay needs (fixture-smtps.mjs reuses its SMTP session).
  start fixture env -i PATH="$PATH" BIND_ADDRESS=10.77.0.4 IMAP_PORT=993 SMTP_PORT=587 HTTPS_PORT=9443 \
    SMTPS_PORT=465 DRIP_SECONDS=100000 ALLOWED_CLIENT_IPS=127.0.0.1,10.77.0.2,10.77.0.3,10.77.0.4 \
    TLS_KEY_DIR="$H/fixture/keys" TLS_PUBLISH_DIR="$H/fixture/public" \
    node "$HERE/fixture-smtps.mjs" "$KPH_FIXTURE"
  wait_tcp 10.77.0.4 465 30

  # KyIdentity: its own server, migrations and bootstrap administrator, behind
  # a TLS front so the issuer is https://id.harness.test.
  mkdir -p "$H/idp/data" "$H/idp/installer"
  chmod 700 "$H/idp" "$H/idp/data" "$H/idp/installer"
  # Its working directory supplies css/ and fonts/, as / does in its image.
  start kyidentity env -C "$KPH_KYIDENTITY_SRC" -i PATH=/usr/bin:/bin HOME="$H/idp" KYIDENTITY_DATA_DIR="$H/idp/data" \
    KYIDENTITY_ISSUER_URL="$ISSUER" KYIDENTITY_LISTEN_HOST=127.0.0.1 KYIDENTITY_PORT=5867 \
    KYIDENTITY_ALLOW_PRIVATE_CALLBACKS=true TRUSTED_PROXY_CIDRS=127.0.0.1 \
    BOOTSTRAP_ADMIN_USER=kph-bootstrap BOOTSTRAP_ADMIN_PASS="$(cat "$H/secrets/idp-bootstrap")" \
    SSL_CERT_FILE="$H/tls/ca.pem" "$H/bin/kyidentity"
  wait_tcp 127.0.0.1 5867 60
  start tls-front node "$HERE/tls-front.mjs" 10.77.0.2:443 127.0.0.1:5867 "$H/tls/fullchain.pem" "$H/tls/privkey.pem"
  wait_tcp 10.77.0.2 443 10
  curl -fsS --cacert "$H/tls/ca.pem" "$ISSUER/.well-known/openid-configuration" >/dev/null \
    || die "KyIdentity discovery failed; see $HOST_DIR/logs/kyidentity.log"

  # KyPost: bootstrap admin, then the installer bundle (SSO + domain challenge).
  mkdir -p "$H/kypost/config" "$H/kypost/state" "$H/kypost/logs" "$H/kypost/secrets"
  chmod 700 "$H/kypost" "$H/kypost"/*
  start_kypost
  printf '{"version":1,"sso":{"enabled":true,"issuerUrl":"%s","clientId":"kypost","clientSecret":"%s","autoProvision":true,"allowInsecureIssuer":false},"pairingSecret":"%s","domains":["%s"]}' \
    "$ISSUER" "$(cat "$H/secrets/client")" "$(cat "$H/secrets/pairing")" "$DOMAIN" \
    | kypost apply-setup --file - >"$H/run/kypost-setup.json"
  # Publish the domain-proof challenge(s) the report returned, exactly as an
  # operator would at their DNS provider.
  jq -r '.dns[] | "txt-record=\(.name),\"\(.value)\""' "$H/run/kypost-setup.json" >"$H/dns/txt.conf"
  [ -s "$H/dns/txt.conf" ] || die "apply-setup returned no DNS challenge: $(cat "$H/run/kypost-setup.json")"
  dns_restart
  stop_pid kypost; start_kypost   # pick up SSO settings cleanly

  sed -n 's/^password: //p' "$H/kypost/config/first-run-password.txt" >"$H/secrets/kypost-bootstrap"
  jq -n --arg origin "$KYPOST_ORIGIN" --arg issuer "$ISSUER" --arg domain "$DOMAIN" \
    --arg ca "$H/tls/ca.pem" --arg home "$H/browser-home" --arg out "$H/shots" \
    '{origin:$origin, issuer:$issuer, domain:$domain, ca:$ca, browserHome:$home, shots:$out,
      relay:{host:"smtp.relay.test", port:465, username:"charlie"}}' >"$H/run/harness.json"
  echo READY >"$H/run/ready"
}

inner_provision() {
  # KyIdentity installer bundle: dedicated admin, everyday owner@harness.test,
  # the kypost OIDC client and its signed suite directory webhook. The webhook
  # signing secret is KyPost's pairing secret (KyPost verifies with it).
  local id
  id=$(cat /proc/sys/kernel/random/uuid)
  local d=$H/idp/installer
  cp "$H/secrets/idp-admin" "$d/admin.password"
  cp "$H/secrets/owner" "$d/owner.password"
  cp "$H/secrets/client" "$d/kypost.secret"
  cp "$H/secrets/pairing" "$d/kypost.webhook"
  chmod 600 "$d"/*
  jq -n --arg id "$id" --arg d "$d" --arg dom "$DOMAIN" --arg origin "$KYPOST_ORIGIN" '{
    schema:"kyidentity.setup/1", setupId:$id,
    admin:{username:"idp-admin", email:("idp-admin@" + $dom), passwordFile:($d + "/admin.password")},
    everyday:{username:"owner", email:("owner@" + $dom), passwordFile:($d + "/owner.password")},
    clients:[{id:"kypost", name:"KyPost", secretFile:($d + "/kypost.secret"),
      redirectUris:[($origin + "/api/auth/oidc/callback")], postLogoutUris:[],
      administratorRole:"kypost.admin", everydayRole:"kypost.user",
      suiteWebhook:{url:($origin + "/api/sync/webhook"), signingSecretFile:($d + "/kypost.webhook")}}]
  }' >"$d/setup.json"
  chmod 600 "$d/setup.json"
  local idp_env=(env -i PATH=/usr/bin:/bin HOME="$H/idp" KYIDENTITY_DATA_DIR="$H/idp/data" KYIDENTITY_ISSUER_URL="$ISSUER")
  as_user "${idp_env[@]}" "$H/bin/kyidentity" apply-setup --file "$d/setup.json" >"$H/run/idp-setup.json"
  as_user "${idp_env[@]}" "$H/bin/kyidentity" apply-setup --file "$d/setup.json" --check >>"$H/run/idp-setup.json"
  as_user "${idp_env[@]}" "$H/bin/kyidentity" resync-system --client kypost --wait 60s >>"$H/run/idp-setup.json" \
    || die "directory delivery to KyPost did not complete: $(tail -1 "$H/run/idp-setup.json")"

  # One-time spool, then the production launcher (hash check, config
  # generation through live admission, Maddy on mx.harness.test:25).
  # Receiving is switched on only now: KyPost refuses it before a domain proves.
  touch "$H/run/receiving-on"
  kypost receiving init >"$H/run/receiving-init.json"
  stop_pid kypost; start_kypost
  install -m 555 "$KPH_MADDY" "$H/bin/maddy"
  local e; mapfile -t e < <(kypost_env)
  start receiver env -i "${e[@]}" KYPOST_NATIVE_RECEIVER=true KYPOST_RECEIVER_BINARY="$H/bin/maddy" \
    KYPOST_RECEIVING_LISTEN=10.77.0.3:25 KYPOST_RECEIVING_HOSTNAME="mx.$DOMAIN" \
    KYPOST_RECEIVING_CERT="$H/tls/fullchain.pem" KYPOST_RECEIVING_KEY="$H/tls/privkey.pem" \
    sh "$REPO/scripts/start-receiving.sh"
  start receiving-server env -i "${e[@]}" "$H/bin/kypost-server" receiving serve
  wait_tcp 10.77.0.3 25 30
  echo PROVISIONED >"$H/run/provisioned"
}

# ---------------------------------------------------------------------------
# Host side
# ---------------------------------------------------------------------------

cmd_up() {
  for c in unshare nsenter dnsmasq openssl certutil jq curl node go ip; do need "$c"; done
  [ -d "${KPH_KYIDENTITY_SRC:-}" ] || die "set KPH_KYIDENTITY_SRC to a KyIdentity-server checkout"
  [ -f "${KPH_FIXTURE:-}/src/index.js" ] || die "set KPH_FIXTURE to a kypost-demo-IMAP checkout with node_modules"
  [ -f "${KPH_MADDY:-}" ] || die "set KPH_MADDY to the pinned Maddy 0.9.5 binary (see docs/DEVELOPMENT.md)"
  [ "$(sha256sum "$KPH_MADDY" | cut -d' ' -f1)" = "$MADDY_SHA" ] || die "KPH_MADDY is not the qualified Maddy 0.9.5 binary"
  [ -f "$REPO/frontend/dist/index.html" ] || die "build the frontend first (cd frontend && npm ci && npm run build)"
  [ ! -e "$HOST_DIR" ] || die "$HOST_DIR exists; run down first"
  mkdir -p "$HOST_DIR"/{bin,dns,logs,run,secrets,shots}
  : >"$HOST_DIR/.kypost-native-harness"  # down deletes only directories carrying this marker
  echo "building kypost-server and kyidentity ..."
  (cd "$REPO/backend" && CGO_ENABLED=0 go build -trimpath -o "$HOST_DIR/bin/kypost-server" ./cmd/)
  (cd "$KPH_KYIDENTITY_SRC" && CGO_ENABLED=0 go build -trimpath -o "$HOST_DIR/bin/kyidentity" ./cmd/kyidentity)
  for s in client idp-bootstrap idp-admin owner; do openssl rand -base64 33 | tr -d '\n=' | tr '+/' '-_' >"$HOST_DIR/secrets/$s"; done
  openssl rand -base64 32 | tr -d '\n' >"$HOST_DIR/secrets/pairing"
  # The holder keeps the namespaces alive; everything else joins them.
  # shellcheck disable=SC2016 # expanded by the inner bash
  KPH_FIXTURE=$KPH_FIXTURE KPH_KYIDENTITY_SRC=$KPH_KYIDENTITY_SRC setsid unshare -rmn -- bash -c 'echo $$ >"$1/ns.pid"; bash "$2" _inner_up "$1" || echo failed >"$1/run/failed"; exec sleep infinity' \
    _ "$HOST_DIR" "$0" >"$HOST_DIR/logs/harness.log" 2>&1 &
  for _ in $(seq 240); do
    [ -f "$HOST_DIR/run/ready" ] && { echo "up: $KYPOST_ORIGIN (inside the namespace); state in $HOST_DIR"; return 0; }
    [ -f "$HOST_DIR/run/failed" ] && break
    sleep 0.5
  done
  tail -20 "$HOST_DIR/logs/harness.log" >&2
  die "phase 1 did not finish"
}

cmd_down() {
  local p ns q
  [ -e "$HOST_DIR" ] || { echo "down: nothing at $HOST_DIR"; return 0; }
  [ -f "$HOST_DIR/.kypost-native-harness" ] || die "$HOST_DIR is not a harness state directory; not deleting it"
  if p=$(cat "$HOST_DIR/ns.pid" 2>/dev/null) && ns=$(readlink "/proc/$p/ns/net" 2>/dev/null); then
    # Every harness process, whichever session started it, shares the holder's
    # network namespace; nothing else does.
    for sig in TERM KILL; do
      for q in $(pgrep -u "$(id -u)"); do
        # It may already have exited (TERM cascades); that must not stop the teardown.
        [ "$(readlink "/proc/$q/ns/net" 2>/dev/null)" = "$ns" ] && { kill "-$sig" "$q" 2>/dev/null || true; }
      done
      [ $sig = TERM ] && sleep 5
    done
  fi
  rm -rf "$HOST_DIR"
  echo "down: removed $HOST_DIR"
}

case "${1:-}" in
  up) cmd_up ;;
  _inner_up) HOST_DIR=$2; inner_up ;;
  provision)
    [ -f "${KPH_MADDY:-}" ] || die "set KPH_MADDY"
    ns_root env KPH_MADDY="$KPH_MADDY" bash "$0" _inner_provision "$HOST_DIR"
    ;;
  _inner_provision) HOST_DIR=$2; inner_provision ;;
  exec) shift; ns_user "$@" ;;
  root) shift; ns_root "$@" ;;
  send)
    [ $# -ge 4 ] || die "usage: send FROM TO SUBJECT [BODY]"
    # shellcheck disable=SC2016 # expanded by the inner bash
    ns_user bash -c 'printf "From: <%s>\r\nTo: <%s>\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@outside.test>\r\n\r\n%s\r\n" \
        "$1" "$2" "$3" "$(date -R)" "$(cat /proc/sys/kernel/random/uuid)" "$4" |
      curl -sS --url "smtp://mx.harness.test:25" --ssl-reqd --cacert /mnt/tls/ca.pem \
        --mail-from "$1" --mail-rcpt "$2" --upload-file - -v 2>&1 | grep -E "^[<>] (MAIL|RCPT|DATA|250|4[0-9][0-9]|5[0-9][0-9])"' \
      _ "$2" "$3" "$4" "${5:-Sent by the native-mail harness.}"
    ;;
  relay-sent)
    # curl reports a custom IMAP command's untagged replies only in its trace.
    ns_user curl -sS -v --cacert /mnt/tls/ca.pem -u charlie:x-any-password \
      "imaps://smtp.relay.test:993/Sent%20Items" -X "FETCH 1:* (BODY.PEEK[HEADER.FIELDS (FROM TO SUBJECT)])" 2>&1 \
      | sed -n 's/^< //p' | sed -n '/^\* [0-9]* FETCH/,/^)/p'
    ;;
  down) cmd_down ;;
  *) sed -n '2,15p' "$0"; exit 2 ;;
esac
