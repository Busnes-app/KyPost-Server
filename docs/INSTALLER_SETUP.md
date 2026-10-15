# Repeatable KyPost setup

KyPost supplies the container-local setup boundary for KyQuickStart gate G7.
The installer owns image pins, target secrets, volumes, environment, edge/TLS,
identity registrations and workload lifecycle. KyPost owns its configuration.
This command does not activate public receiving, edit DNS or assign identities.
The native-mail production qualification gates in `TURNKEY_MAIL_STACK_PLAN.md`
still apply. Recovery and capacity qualification follow the full redeployment.

## Clean installation sequence

1. Prepare the supported Linux x86-64 target, private data volumes and pinned
   KyPost image through KyQuickStart. Keep `KYPOST_BIND` explicit; configure
   inbound TLS or the selected proxy and its exact trusted addresses. Set
   `SERVER_BASE_URL` to the external HTTPS origin. Retain external IMAP mode
   for existing accounts; native installation uses new identities.
2. Start the base image once. Bootstrap creates the recovery administrator and
   runtime keys. Retrieve the initial credential through the existing first-start
   procedure, outside installer state/logs. Register KyIdentity's OIDC client,
   `kypost.admin` role, assigned-users-only access and webhook system using the
   retained target pairing key. Keep the dedicated administrator separate from
   the everyday identity. Never replace a key generated on a previous run.
3. Run `apply-setup` as the runtime user with the private target bundle below.
   This initializes SSO and domain challenges. Publish the returned TXT records
   and verify them through Server → Mail domain. The records are public proof
   challenges, not SMTP credentials. Keep them published.
4. Configure the operator-owned relay in Server → Mail domain and run Check
   saved relay. Configure the bulk repository and independent backup destination
   through the installer; pair KyRecovery after its custodian ceremony. Run the
   first backup and inspect its receipt. Setup never claims a TLS/AUTH check is
   actual message delivery or a local backup copy is independent.
5. Assign the everyday identity. Confirm its native mailbox appears. Initialize
   only a new receiving spool, then select either the bundled receiver or hosted
   adapter. The direct-receiver profile needs a protected pinned engine and TLS
   files plus independently reachable inbound port25; the hosted profile needs
   its separately qualified provider resources. Use `scripts/setup-mail.sh` and
   `docs/RECEIVING_SETUP.md` for the controlled standalone Compose path.
6. Run `setup-status`. Resolve every unconfigured check. Then perform its external
   checks: HTTPS/proxy behavior, receiver TLS/reachability, actual incoming mail,
   outgoing recipient receipt/authentication headers/Sent, and independent backup
   and restore. Record these observations separately. Configuration success is
   not deployment acceptance. Public MX cutover needs its own approved plan.

## Container-local command

```sh
docker compose exec -T --user kypost kypost-server \
  kypost-server apply-setup --file /run/kyquickstart/kypost-setup.json
```

The file must be a runtime-owned regular owner-only file without symlinks,
maximum64KiB. Mount/copy it privately on the target, never into workstation
state, command arguments or logs. An installer can instead stream its target-side
bundle through protected exec stdin:

```sh
docker compose exec -T --user kypost kypost-server \
  kypost-server apply-setup --file - < /protected/target/kypost-setup.json
```

Here the shell and input file are on the target. KyQuickStart uses its SSH/exec
transport; secrets must not be read into saved workstation state. Kubernetes
uses the same command with pod exec stdin and the runtime UID. Do not change
volume ownership merely to make the command run as root.

Version1 JSON, with optional sections (at least one required):

```json
{
  "version": 1,
  "sso": {
    "enabled": true,
    "issuerUrl": "https://identity.example.com",
    "clientId": "kypost-client-id",
    "clientSecret": "<target-held OIDC secret>",
    "autoProvision": true,
    "allowInsecureIssuer": false
  },
  "pairingSecret": "<retained effective runtime pairing secret>",
  "domains": ["example.com"],
  "recovery": {
    "url": "https://recovery.example.com",
    "code": "<fresh six-digit pairing code>"
  }
}
```

Omit sections not yet available. Pairing-key setup is optional because base
bootstrap already generates it; supplying it verifies the existing effective key.
The pairing secret is a witness, never a key-creation request. It must match
`api.ReadPairingSecret` authority: the bootstrap-generated 32-byte key encoded
as standard base64, or the trimmed `PAIRING_SECRET` override when set. Missing
or weak effective authority refuses; the installer never originates a key.
Recovery can be a separate later bundle; an already paired matching destination needs no code, while an
unpaired server needs a fresh one. Preserve the recovery key pin and verify its
fingerprint against the custodian record through the existing ceremony.
Private recovery URLs need the deployment's existing explicit
`KYPOST_BACKUP_ALLOW_PRIVATE_RECOVERY` opt-in (see `.env.example`).

The report is secret-free JSON: `version`, `applied`, `unchanged`, `dns`, `pending`
and `configurationApplied`. Success means requested configuration was applied
or retained. It never means the server exchanged mail or is production-ready.

## Reruns and failures

- Existing identical SSO/key settings are retained byte-for-byte; differing or
  unreadable settings refuse before requested writes. Change existing settings
  through the protected UI, not installer setup.
- Existing domain challenges and established state are retained. Setup never
  rotates TXT records or reactivates a retired domain; other domains remain.
- Recovery pairing checks the ordinary cross-process backup fence. An existing
  matching pairing retains its token/key without consuming another code; a
  different destination refuses. A consumed code whose response was lost needs
  operator inspection and a new code if no pairing was durably recorded.
- Every requested change has intent/completion audit rows in the ordinary
  instance audit store. An error may follow an already committed earlier section;
  inspect retained settings and rerun after remediation. No rollback deletes
  credentials, domains or mail. A completion-audit failure explicitly requires
  inspection before retry.
- Run under the installer's exclusive target lock, with no concurrent UI
  configuration/restore or second installer. Domain/recovery stores and file
  publication have their own disk fences; this command grants no session or role.
- Restore-held instances refuse setup. Follow the guarded recovery procedure;
  never remove a hold to resume installation.
- Unknown fields, trailing JSON, oversized input, unsafe files, malformed keys,
  invalid domains and non-HTTPS SSO refuse. Error messages do not echo bundle
  contents, provider responses or secrets.

## Configuration status

```sh
docker compose exec -T --user kypost kypost-server kypost-server setup-status
```

JSON `checks` contains stable IDs: `public-origin`, `identity`, `webhook-key`, `native-mail`,
`mail-domains`, `restore-authority`, `relay`, `receiving`, `bulk-backup`,
`sealed-backup`, `backup-schedule`, `backup-receipt`, `backup-result`. Each failed configuration
check includes remediation. `backup-result` requires the latest relevant attempt
in the recent audit window to be a successful ordinary backup; failures, local
fallbacks, incomplete intents and absent recent evidence require inspection. `readyForExternalChecks` means these local
configuration observations pass, not that external acceptance passed.
`externalChecks` always lists the remaining independent observations.

Status is advisory and does not refresh DNS, submit mail, test credentials or
change settings. It opens existing SQLite state read-only and uses lock-free backup inspection,
which may maintain SQLite WAL companions but creates no application lock files. Domain established state,
receiving-profile configuration and an old backup receipt are not live runtime,
fresh delivery or restore proof. Through the UI also inspect Health, outbox
uncertainty, receiving/spool diagnostics and the latest backup result.

The installer must validate `configurationApplied` when applying the bundle,
then every status check and external acceptance separately; it must not use a
successful command exit alone as an install-complete marker. This product gate
opens only after the feature is merged, released/tagged and included in the
installer's verified pinned release set.

## Installer discovery and inspection

`apply-setup --capabilities` advertises `ky.setup-capabilities/1`, service `kypost`
and native bundle `kypost.setup/1` (the JSON retains `version: 1`).
`apply-setup --file <private-file|-> --check` observes the requested settings;
it fails if anything is absent or differs, without publishing configuration, auditing,
creating application locks, migrating state or claiming a recovery code. `setup-status`
uses the same existing read-only database and lock-free backup inspection. SQLite may
maintain live WAL shared-memory companions; checks are advisory across independent files.

Use top-level `clientSecretFile` beside `sso`, `pairingSecretFile` beside the retained
pairing-secret witness, and `recovery.codeFile` instead of the corresponding inline
values. References must be absolute runtime-owned private regular files without final
symlinks, at most 4096 bytes. One final newline is removed. Inline and file values
cannot both be present. Native protected inline/stdin bundles remain supported.
Generated `pairing.key` bytes are not the effective base64 pairing witness: register
the retained effective secret through the existing product contract.

Docker execution must use `--user kypost`, including capabilities and checks. This
interface does not provision identity grants: KyIdentity app role `kypost.admin` and
signed directory replication remain the authority. New directory/browser identities
and authorized links now retain a separate issuer binding; legacy unbound identities
are bound to the configured issuer when the API starts, never by the installer, and
cannot be claimed as installer-owned.
External-IMAP administrator separation and full signed suite qualification remain gates.

The public `GET /api/installer/network` returns exactly `clientIp`,
`proxyHeadersTrusted` and `scheme`, with no-store. It reuses normal product trust and
security decisions; `scheme` includes the configured `SERVER_BASE_URL` HTTPS fallback,
so it is an effective-security observation rather than independent backend TLS proof.
Successful configuration still reports pending DNS, relay, receiving and independent
recovery acceptance; those prerequisites are not erased by a successful `--check`.

Enabled issuer URLs must be canonical, without surrounding whitespace or trailing slash,
so directory bindings and the OIDC discovery issuer remain identical. Existing noncanonical
settings require an explicit protected configuration correction; setup never rewrites them.
