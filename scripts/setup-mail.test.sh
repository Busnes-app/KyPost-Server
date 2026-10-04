#!/usr/bin/env bash
# Exercise definitions only: never open browsers, build/start containers or touch real .env.
set -euo pipefail
setup_test_dir=$(mktemp -d)
trap 'rm -rf "$setup_test_dir"' EXIT
awk '/^# BEGIN INTERACTIVE STAGES$/ {exit} {print}' "$(dirname "$0")/setup-mail.sh" > "$setup_test_dir/definitions.sh"
# shellcheck source=/dev/null
source "$setup_test_dir/definitions.sh"
validate_settings https://post.example.com /opt/maddy /etc/tls mail.example.com 127.0.0.1 2525
validate_settings 'http://[::1]:5866' /opt/maddy /etc/tls mail.example.com '[::1]' 25
for setup_bad_url in 'http://post.example.com' 'https://user:secret@post.example.com' 'https://post.example.com/path' 'https://post.example.com:70000' "https://post.\$VALUE"; do
  if validate_settings "$setup_bad_url" /opt/maddy /etc/tls mail.example.com 127.0.0.1 25 >/dev/null 2>&1; then exit 1; fi
done
for setup_bad_bind in '999.1.1.1' '::1' '[127.0.0.1]' 'localhost' '[::1%eth0]' "[::1%\${VALUE}]" $'[::1%\nZONE=value]'; do
  if validate_settings https://post.example.com /opt/maddy /etc/tls mail.example.com "$setup_bad_bind" 25 >/dev/null 2>&1; then exit 1; fi
done
for setup_bad_port in 0 025 65536 xyz; do
  if validate_settings https://post.example.com /opt/maddy /etc/tls mail.example.com 127.0.0.1 "$setup_bad_port" >/dev/null 2>&1; then exit 1; fi
done
for setup_bad_host in 'Mail.example.com' 'mail..example.com' '-mail.example.com' 'mail.example.com.' '*.example.com'; do
  if validate_settings https://post.example.com /opt/maddy /etc/tls "$setup_bad_host" 127.0.0.1 25 >/dev/null 2>&1; then exit 1; fi
done
for setup_bad_path in relative '/path with space' "/path\$VALUE" '/path#comment' $'/path\nKEY=value'; do
  if validate_settings https://post.example.com "$setup_bad_path" /etc/tls mail.example.com 127.0.0.1 25 >/dev/null 2>&1; then exit 1; fi
done
mkdir "$setup_test_dir/tls"
printf 'never executed\n' > "$setup_test_dir/engine"
chmod 555 "$setup_test_dir/engine"
printf 'metadata fixture only, not TLS qualification\n' > "$setup_test_dir/tls/fullchain.pem"
cp "$setup_test_dir/tls/fullchain.pem" "$setup_test_dir/tls/privkey.pem"
chmod 644 "$setup_test_dir/tls/privkey.pem"
if validate_files "$setup_test_dir/engine" "$setup_test_dir/tls" >/dev/null; then exit 1; fi
chmod 600 "$setup_test_dir/tls/privkey.pem"
if validate_files "$setup_test_dir/engine" "$setup_test_dir/tls" >/dev/null; then exit 1; fi
mv "$setup_test_dir/tls/fullchain.pem" "$setup_test_dir/tls/original.pem"
ln -s original.pem "$setup_test_dir/tls/fullchain.pem"
if validate_files "$setup_test_dir/engine" "$setup_test_dir/tls" >/dev/null; then exit 1; fi
ENV_FILE="$setup_test_dir/.env"
printf 'UNCHANGED=sentinel\nKYPOST_BIND=old\n' > "$setup_test_dir/example"
prepare_env "$setup_test_dir/example"
[[ $(stat -c %a "$ENV_FILE") == 600 ]]
write_env KYPOST_BIND 127.0.0.1 >/dev/null
write_env KYPOST_BIND 192.0.2.1 >/dev/null
[[ $(grep -c '^KYPOST_BIND=' "$ENV_FILE") == 1 ]]
grep -q '^KYPOST_BIND=192.0.2.1$' "$ENV_FILE"
grep -q '^UNCHANGED=sentinel$' "$ENV_FILE"
# Real Compose parsing only; no build, container, listener or browser is started.
# Globals are consumed by the sourced Compose wrapper definitions.
export setup_repo setup_url KYPOST_BIND KYPOST_MADDY_BINARY KYPOST_RECEIVING_TLS_DIR KYPOST_RECEIVING_HOSTNAME KYPOST_SMTP_BIND KYPOST_SMTP_PORT
setup_repo=$(cd -- "$(dirname -- "$0")/.." && pwd)
setup_url=https://post.example.com
KYPOST_BIND=127.0.0.1
KYPOST_MADDY_BINARY=/opt/qualified-maddy
KYPOST_RECEIVING_TLS_DIR=/etc/receiver-tls
KYPOST_RECEIVING_HOSTNAME=mail.example.com
KYPOST_SMTP_BIND='[::1]'
KYPOST_SMTP_PORT=2525
# Conflicting inherited values must not defeat the operator's selected inputs.
export SERVER_BASE_URL=https://stale.example.com KYPOST_NATIVE_MAIL=false KYPOST_NATIVE_RECEIVING=true
compose_base config --format json > "$setup_test_dir/base.json"
compose_receiving config --format json > "$setup_test_dir/receiving.json"
python3 - "$setup_test_dir/base.json" "$setup_test_dir/receiving.json" <<'CHECK'
import json, sys
base, receiving = [json.load(open(path))['services']['kypost-server'] for path in sys.argv[1:]]
assert base['environment']['SERVER_BASE_URL'] == 'https://post.example.com'
assert base['environment']['KYPOST_NATIVE_MAIL'] == 'true'
assert base['environment']['KYPOST_NATIVE_RECEIVING'] == 'false'
assert len(base['ports']) == 1 and base['ports'][0]['host_ip'] == '127.0.0.1'
assert receiving['environment']['SERVER_BASE_URL'] == 'https://post.example.com'
assert receiving['environment']['KYPOST_NATIVE_RECEIVING'] == 'true'
assert receiving['environment']['KYPOST_NATIVE_RECEIVER'] == 'true'
assert any(p['host_ip'] == '::1' and p['target'] == 2525 for p in receiving['ports'])
assert all(v['read_only'] and not v.get('bind', {}).get('create_host_path', False) for v in receiving['volumes'] if v['type'] == 'bind' and v['target'].startswith('/opt/kypost/receiving/'))
CHECK
chmod 644 "$ENV_FILE"
if prepare_env "$setup_test_dir/example" >/dev/null; then exit 1; fi
rm "$ENV_FILE"
ln -s "$setup_test_dir/missing-target" "$ENV_FILE"
if prepare_env "$setup_test_dir/example" >/dev/null; then exit 1; fi
[[ ! -e "$setup_test_dir/missing-target" ]]
rm "$ENV_FILE"
mkdir "$ENV_FILE"
if prepare_env "$setup_test_dir/example" >/dev/null; then exit 1; fi
printf 'ok: public-input rejection, private dotenv creation/upsert and symlink refusal; no interactive stages run\n'
