# Controlled direct receiver setup

This is an opt-in Linux x86_64 qualification profile for a dedicated test
domain. It receives SMTP directly into KyPost's durable holding store, then
the daemon imports into native mailboxes without IMAP. Public production MX,
automatic receiver packaging and complete turnkey installation remain gated
by [receiver qualification](RECEIVING_GATEWAY_ASSESSMENT.md).

## Prepare the existing stack

1. Use a new native deployment with `KYPOST_NATIVE_MAIL=true` before assigning
   people through KyIdentity. Pair KyIdentity, configure Server → Mail domain,
   publish the exact `_kypost-mail` TXT challenge and verify it. Keep that TXT
   record in place. Assign test users with explicit primary addresses in this
   domain; verify their native mailboxes were prepared. Existing IMAP accounts
   are never adopted automatically.
2. Provide the operator-owned outgoing relay through Server → Mail domain.
   The saved-relay check tests TLS/AUTH only. Use the
   [AWS/Cloudflare test matrix](TURNKEY_MAIL_STACK_PLAN.md#operator-owned-relay-test-matrix)
   to qualify delivery separately. No provider credentials belong in receiver
   configuration.
3. Run all commands below as the same unprivileged OS user and in the same
   filesystem/environment as KyPost, with absolute `CONFIG_DIR` and `STATE_DIR`.
   Both roots must already exist with owner-only permissions. The container image
   and entrypoint enforce `0700` on config/private/state roots, including mounted
   volumes; a host installation must arrange the same permissions itself. Initially leave
   the daemon receiving flag false. Run
   `KYPOST_NATIVE_RECEIVING=true kypost-server receiving init` explicitly once
   after domain setup, then configure `KYPOST_NATIVE_RECEIVING=true` for the
   daemon and receiver command environment and restart the daemon. Initialization refuses an existing spool;
   never repeat it to repair missing accepted mail. A restore hold refuses use
   and currently requires separate reconciliation work.
4. Supply the separately installed, pinned Maddy executable from
   [the assessment](RECEIVING_GATEWAY_ASSESSMENT.md#pinned-executable-and-runnable-check).
   KyPost does not download or distribute it. Keep executable and parent paths
   trusted and read-only to mail-processing processes. The measured hash pins
   content; it does not independently verify upstream provenance or satisfy
   distribution licensing obligations.
5. Supply readable regular PEM certificate/full-chain and private-key files.
   The key must have owner-only permissions. Paths must be absolute, without
   symlinks, controls, quotes, backslashes, braces or dollar signs. Copy renewed
   files into protected regular-file paths atomically. The explicit lowercase
   hostname must match a currently valid server certificate. Generation checks
   the pair, name, dates and server purpose; it does not establish public CA
   trust, complete-chain validity or successful renewal. Qualify these with a
   real client. A private test CA requires that client to trust it explicitly.

## Generate and run

Set `RECEIVING_LISTEN` to an explicit literal `IP:port`, `RECEIVING_HOSTNAME`
to the public/test hostname, `RECEIVING_CERT` and `RECEIVING_KEY` to the protected
TLS files, and `MADDY_BINARY` to the qualified binary path. These shell variables
are command inputs, not new KyPost environment settings. For an initial local
test use a loopback high port; no privileged user is needed.

```sh
set -eu
umask 077
printf '%s  %s\n' \
  6ea4b951f15b91fd81d98957e4d4bad7a0cec6d6e1d66b011c765cc9a14e05db \
  "$MADDY_BINARY" | sha256sum -c -
receiver_config_tmp=$(mktemp "$CONFIG_DIR/receiving-config.XXXXXX")
if kypost-server receiving config "$RECEIVING_LISTEN" "$RECEIVING_HOSTNAME" \
    "$RECEIVING_CERT" "$RECEIVING_KEY" > "$receiver_config_tmp"; then
  mv "$receiver_config_tmp" "$CONFIG_DIR/receiving.conf"
else
  rm -f "$receiver_config_tmp"
  exit 1
fi
( ulimit -n 256; exec "$MADDY_BINARY" --config "$CONFIG_DIR/receiving.conf" run )
```

Generation performs existing-only storage admission, fresh TXT verification,
and a final domain/issuer/restore check before emitting complete configuration.
Validation failure emits no configuration; the temporary-file pattern preserves an older
configuration after failure. Successful generation is historical evidence,
not continuing authority or permission to change MX. RCPT and DATA continue
to verify current owners and fresh domain proof. Changing the challenge can
pause reception until the matching DNS value propagates.

The foreground process inherits the native file-descriptor ceiling, bounding
idle socket descriptors as well as active transactions. Give it explicit
supervision, restart and log rotation before any remote test; the existing
KyPost container does not supervise it automatically. Route a dedicated test
domain to it only after checking TLS, backups, firewall and SMTP port 25
reachability. HTTP reverse proxies do not carry SMTP. Preserve prior DNS and
keep the existing provider route available for rollback.

## Fixed qualification policy and limits

- STARTTLS is mandatory before MAIL/RCPT; TLS versions 1.2 and 1.3 only. Senders
  unable to use TLS are refused. This is stricter than ordinary opportunistic
  SMTP and deliberately limits compatibility; there is no plaintext switch.
- Maximum message 4 MiB (including receiver-added headers), headers 128 KiB,
  recipients 100. RAM buffering avoids temporary payload spool files; up to
  four active messages can consume roughly 16 MiB plus parsing/process overhead.
- Four active transactions overall, two per IP; bursts of 30/minute overall
  and 10/minute per IP. Limit waits are bounded by the engine's five-second
  acquisition timeout. Read/write/shutdown timeouts are 30 seconds. Active
  transaction limits do not bound idle sockets without the OS descriptor cap.
- Only the verified mail domain is routed, with no SMTP AUTH, outbound queue
  or external relay target. The last RCPT check freezes ownership; the BODY
  check commits MIME before success. The dummy target runs only after that
  durable check. Removing/reordering these checks can discard accepted mail.
- Debug/wire logging stays disabled. Maddy logs envelope/IP/transaction and
  command metadata to stderr; protect and rotate those logs. Its command module
  discards helper stderr, so individual helper JSON diagnostics are unavailable
  through this bridge. Daemon import errors remain in KyPost's normal logs.
- Rate/size limits are not spam authentication or filtering. Sender-written
  authentication headers are untrusted. Hard volume quotas, safe reservation
  cleanup, provenance/licensing and intended-volume power-loss/restore checks
  remain public release gates. Never erase receipts or accepted mail to free space.

## Verify and roll back

Use the actual pinned-Maddy test from the assessment. It consumes generated
production configuration and proves STARTTLS, plaintext refusal without
bindings, external-recipient refusal, per-IP concurrent and burst refusal, durable
two-owner delivery and import after receiver SIGKILL/restart under closed storage admission. This is not a storage-process restart or power-loss proof. It uses test-only
loopback DNS and a private CA; it is not live-provider evidence.

For the dedicated domain, send ordinary and PGP mail from a controlled external
account, inspect both KyPost mailboxes and raw attachments, and reply through
the configured operator relay. Check actual recipient receipt independently of
relay acceptance. Stop/restart receiving and importing without deleting storage;
accepted mail must survive. Confirm disabled recipients and DNS/storage failure
refuse new reception. Record the domain, relay, recipient and observed results
without credentials.

Rollback: stop new reception first, restore prior test-domain DNS, retain a
compatible KyPost binary and all native state, and drain or reconcile pending
and quarantined deliveries. DNS rollback changes future routing only. Disabling
the receiver or native flags does not migrate native mail to an IMAP service.
