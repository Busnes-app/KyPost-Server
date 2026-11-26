#!/usr/bin/env bash
# Runs inside the container started by run.sh. Prints one line per tool and file.
set -uo pipefail
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null && apt-get install -yqq gnupg rnp sq >/dev/null 2>&1
echo "gpg $(gpg --version | head -1 | awk '{print $3}'), rnp $(rnp --version 2>&1 | head -1 | awk '{print $2}'), sq $(sq version 2>&1 | head -1 | awk '{print $2}')"
for f in v4 v6-ed25519 v6-pqc bundle-v4-v6pqc bundle-v6pqc-v4 bundle-v4-v6; do
  g=$(mktemp -d) r=$(mktemp -d)
  gpg --homedir "$g" --batch --import "/w/$f.pgp" >/dev/null 2>&1
  gpg_keys=$(gpg --homedir "$g" --batch --list-keys --with-colons 2>/dev/null | grep -c '^pub')
  gpg --homedir "$g" --batch --import /w/v4.pgp >/dev/null 2>&1
  gpg_ok=$(gpg --homedir "$g" --batch --list-keys >/dev/null 2>&1 && echo yes || echo no)
  rnpkeys --homedir "$r" --import "/w/$f.pgp" >/dev/null 2>&1
  rnp_keys=$(rnpkeys --homedir "$r" --list-keys 2>/dev/null | grep -c '^pub')
  sq_certs=$(sq inspect "/w/$f.pgp" 2>/dev/null | grep -c 'Fingerprint:')
  printf '%-18s gpg imported %s (keyring usable after: %s), rnp imported %s, sq parsed %s\n' \
    "$f" "$gpg_keys" "$gpg_ok" "$rnp_keys" "$sq_certs"
done
