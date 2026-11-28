# A receiving worker in each cell, design

Date: 2026-11-28. Status: proposed. The build is a follow-up PR stacked on this one; see "As
built".

The owner said on 2026-11-28 that the receiving worker must be rebuilt because it is blocked on
the Cloudflare runtime's post-quantum support, and that "it wouldn't hurt to have a worker running
in each cell to save on the Cloudflare bill". This note names the blocker exactly, then designs the
per-cell receiver.

Cited specs: **RX** is the hosted receiving tier
(`2026-11-21-hosted-receiving-tier-design.md`), **MH** is multi-host replicas
(`2026-11-22-multi-host-replicas-design.md`, MH10 is its cross-region relay routing), and **PQ**
is the stack PQC plan (row numbers from its plan table). Merged #761 moved the RX transport to
ML-DSA-65 keys. Paths are relative to the repository root. "Unverified" marks anything not run or
read in a primary source tonight.

## Summary

- **The blocker is ML-DSA in workerd, plus TLS control.** The pinned workerd (`1.20261119.1`,
  through Miniflare `5.20261119.0-beta` in `worker/`) has no ML-DSA in WebCrypto or `node:crypto`,
  so the Worker cannot verify a post-quantum signature on its routing table. A Worker also cannot
  hold an ML-DSA key, either as a TLS server identity or as a client certificate. So it cannot
  join the receiving tier's pinned ML-DSA-65 mTLS.
- **The receiver in each cell already exists in Go.** It is RX's MX host: pinned Maddy 0.9.5
  plus the `kypost-server receiving bind|accept` helper (`docker-compose.mx.yml`). That helper
  already reaches its cell's replicas over hybrid-only TLS 1.3 with ML-DSA-65 pinned on both
  sides, and reaches other cells through MH10. This note makes it the per-cell receiving worker.
  No new runtime.
- **One gap was real.** The pinned Maddy binary ships with Go's hybrid key exchange turned off:
  its `DefaultGODEBUG` has `tlsmlkem=0`. So public STARTTLS negotiated classical X25519 even with
  a sender that offered `X25519MLKEM768`. The build turns the hybrid on in the launcher and proves
  it against the real binary.
- **Cloudflare becomes optional.** Hosted cells never use it (RX R1). Single-tenant installs keep
  the Cloudflare continuous profile as an option, documented as classical-signed until workerd
  ships ML-DSA.
- **Memory per cell, measured:** about 550 MiB peak per MX host at the default 4 concurrent
  25 MiB transactions. The build caps the container at 768 MiB with no swap. Two MX hosts per
  cell.
- **Fail-safe is RX's rule, unchanged:** `250` only after PostgreSQL committed, and an MX host
  holds nothing. Every failure before `250` is a dropped session or a `451`, and the sender
  retries.

## The blocker, hop by hop

The Cloudflare continuous profile (`docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md`) has four hops.

| Hop | What protects it | Post-quantum state |
|---|---|---|
| 1. Sending MTA to Cloudflare's MX | Cloudflare's SMTP TLS | Unverified. Outbound port 25 is blocked on the test host, so `route1.mx.cloudflare.net` could not be probed. It is not ours to change in any case. |
| 2. Worker to R2 | Cloudflare internal | Not observable. |
| 3. KyPost pickup to the Worker (`cfreceiving` client) | HTTPS to the Cloudflare edge, a 256-bit bearer | **Key exchange is hybrid.** `openssl s_client -groups X25519MLKEM768 -connect workers.dev:443` (OpenSSL 3.5.9) negotiated `X25519MLKEM768`. **Authentication is classical:** the edge presents a public-CA `ecdsa_secp256r1_sha256` certificate, and KyPost authenticates with a bearer. There is no pinned key either way. |
| 4. Routing table KyPost to Worker (`PUT /routes`, `POST /rotate`) | Ed25519 signature verified **inside the Worker** (`continuous.mjs`, `cfreceiving/wire.go:58-88`) | **Blocked.** PQ row 41 requires ML-DSA-65 plus Ed25519, and the Worker cannot verify ML-DSA. Probe below. |

**Probe (2026-11-28, Major, disposable Miniflare, compatibility date `2026-09-18`).** The same
probe ran with no flags and with `nodejs_compat`. Both gave identical results:

| Call inside workerd | Result |
|---|---|
| `crypto.subtle.generateKey({name: "ML-DSA-65"})` | `NotSupportedError: Unrecognized key generation algorithm "ML-DSA-65"` |
| `crypto.subtle.generateKey({name: "ML-DSA-44"})` | the same error |
| `crypto.subtle.generateKey({name: "ML-KEM-768"})` | the same error |
| `crypto.subtle.generateKey({name: "Ed25519"})`, `X25519` | ok |
| `node:crypto` `generateKeyPairSync("ml-dsa-65")` | `The argument 'type' must be one of: 'rsa', 'ec', 'ed25519', 'x25519', 'dh'` |

Node 26.13.1 (OpenSSL 3.5.9) on the same host supports `ML-DSA-65` in both WebCrypto and
`node:crypto`. The gap is workerd's, not JavaScript's.

**Why RX's #761 requirement cannot be met on Cloudflare at all:** RX requires TLS 1.3 restricted
to `X25519MLKEM768` (`pqtls.HybridOnly`), with ML-DSA-65 keys pinned by SPKI on both sides
(`backend/internal/app/receiving_tls.go`). A Worker cannot meet any part of it:

- Its inbound TLS ends at Cloudflare's edge, with Cloudflare's certificate.
- Outbound `connect()` and `fetch()` offer no client key and no group list.
- It cannot verify an ML-DSA signature.

A bundled JavaScript or WASM ML-DSA would close hop 4 only. It is also outside the Worker's
dependency rule (`receiving-worker/AGENTS.md`), and its CPU cost against Workers limits is
unverified. Hop 3's server authentication would stay classical.

## Decisions (defaults; flag any you disagree with)

| # | Decision | Why |
|---|---|---|
| W1 | **The per-cell receiving worker is RX's MX host:** Go `kypost-server` helper plus pinned Maddy 0.9.5, from the same image. Not workerd, and not a Node rewrite. | It already meets #761, and its `bind` and `accept` checks are the ones the replicas run. A Node worker could do ML-DSA but would duplicate every check in a second language. workerd cannot (above). |
| W2 | **Public SMTP offers `X25519MLKEM768` and stays open to classical senders.** | Senders are third parties, and most are classical. Refusing them refuses mail. The hybrid protects every sender that offers it. |
| W3 | **Hosted cells never use Cloudflare, not even as a fallback MX.** | RX R1 (Cloudflare's view of every customer address), and hops 3 and 4 above. A fallback would need an owner decision that overturns R1. |
| W4 | **Single-tenant keeps the Cloudflare continuous profile, unchanged.** | It is how self-hosters behind a blocked port 25 receive mail. Its routing signatures stay Ed25519 until workerd ships ML-DSA (PQ row 41 stays open). |
| W5 | **Two MX hosts per cell, at equal MX preference, each a `docker-compose.mx.yml` project.** `KY_CELLS` names that one cell. `remote.json` names its own replicas, plus the other cells and the relay domain (MH10). | RX "MX host" and "`250` after commit, and MX failover": one host can die without losing mail. |
| W6 | **Compose is the deployment.** ky-kubernetes manifests are a follow-up in that repo. | `docker-compose.mx.yml` and its CI check exist here. ky-kubernetes is stale (PQ row 4) and is not in this repo. |
| W7 | **Memory: 768 MiB cap, no swap, at the default limits.** | Measured below. An operator who raises `KYPOST_RECEIVING_CONCURRENCY` raises the cap by about 140 MiB per extra transaction. |

## How inbound mail reaches it

Nothing changes here from RX and MH10:

- **Own domains.** Each customer domain's MX records name its own cell's MX hosts, as RX already
  documents.
- **Relay domain.** The shared relay domain's MX records name every cell's MX hosts. `bind` for a
  relay address goes to the cell its region code names (MH10, `receivingRemote.route`).
  Everything else, including unknown codes, goes to the host's own cell.
- **Completion.** `accept` asks every cell with `held`, and sends the body only to the cells that
  hold the transaction. The answer is `250` only when all of them commit.

**This PR changes no DNS or MX records, and nothing is deployed.** Relay codes stay unassigned,
an owner decision recorded in `kypost-multihost-build`.

## Memory per cell (measured)

Measured on Major on 2026-11-28 with the pinned Maddy (`sha256 6ea4b951…`) and a dev build of
`kypost-server` from `origin/main` `2887cea1`:

| Process | Load | Peak RSS |
|---|---|---|
| Maddy | idle | 25 MiB |
| Maddy | 4 concurrent STARTTLS transactions of 24.6 MB each (`buffer ram`), with a stub helper holding each body | 236 MiB |
| `kypost-server receiving accept` (MX mode, `KYPOST_RECEIVING_REMOTE` set) | one 24.6 MB body, read in full before forwarding | 78 MiB |
| `kypost-server receiving bind` (MX mode) | one recipient | 26 MiB |

The total at the defaults (4 transactions in all, 2 per IP) is:

- Maddy 236 MiB;
- four accept helpers, 312 MiB;
- the tmpfs ceilings, 76 MiB.

That is about 620 MiB in the worst case, so the cap is 768 MiB. Binds run one at a time within a
transaction and finish before `accept`. A CX23 (4 GB RAM per Hetzner's listing, not re-checked
tonight) holds one MX host with room to spare. Placement is RX's: a small host that holds no mail.
Running the MX container on an existing cell node instead would save a server but put port 25 on
it. That is an owner call; the default is RX's placement.

The Cloudflare bill this removes for hosted mail is Email Routing, Workers requests and R2 storage
for every hosted message. Hosted mail already never used Cloudflare by design (R1). The saving is
real only where the owner runs the Cloudflare profile today (unverified).

## Fail-safe: no acknowledged mail lost

These rules are all RX's and MH10's, already built. The build adds none.

- `accept` exits 0, and Maddy answers `250`, only after `holding.Accept` committed to PostgreSQL.
  A synchronous standby is required once PostgreSQL is replicated (RX R5).
- An MX host holds nothing: the profile and state are in tmpfs, with no volume. If it dies or is
  OOM-killed before `250`, the session drops and the sender retries, usually against the other MX
  host.
- Replicas or PostgreSQL down: the helper exits 1 and Maddy answers `451 4.3.0`. There is no
  local fallback (`TestReceivingRemoteHasNoLocalFallback`).
- A lost answer after a commit gives `451`, and the sender retries. That is at least once and
  never lost (RX).
- Across cells, `250` comes only when every holder committed. Otherwise `451`
  (`TestCrossCellRelayFailureInOneCellIsTemporary`).

Compare the Cloudflare profile: there Cloudflare answers `250` once R2 holds the message, so
acknowledged mail depends on R2 until KyPost picks it up.

## How it is tested

### The existing Worker tests, ported

The Worker's tests are `receiving-worker/*.test.mjs`. Each property maps to the per-cell MX path
as follows. Properties of the signed table and of pickup do not port: the MX host has no table
(`bind` reads routes on the replica) and holds no mail, so those attacks have no target.

| Worker test (continuous unless marked) | Per-cell MX equivalent |
|---|---|
| capture persists exact bytes; pickup digest-deletes | `TestReceivingTLSForwardsUnderThePinnedGateway`, `TestCrossCellRelayDelivers` (bytes reach the replica's ingress store) |
| refusals before the body is read, nothing stored | `bind` at `RCPT`: `TestHostedBindForeignOrg`, `TestNativeReceivingRefusesRevokedRecipientBeforeData`, `TestCrossOrgMatrixSMTPRcpt` |
| capture resolves only after the conditional put | `250` after commit: `TestReceivingTLSExitCodesAndHostedProfile`, `TestNativeReceivingCommitsFrozenRecipientsWithoutIMAP` |
| storage failure throws instead of rejecting | `TestReceivingTLSHungReplica`, `TestReceivingRemoteHasNoLocalFallback`, `TestCrossCellRelayFailureInOneCellIsTemporary` (all temporary) |
| capture never overwrites | `TestReceivingTLSReplayOfArchivedAccept` |
| bearer before any read; strict paths, HTTPS | pinned ML-DSA-65 mTLS before any frame: `TestReceivingTLSRefusesUnpinnedKeysBeforeAnyFrame`, `TestReceivingTLSHelperRefusesAWrongServerKey`, `TestReceivingTLSRefusesBadFrames`, `TestReceivingTLSRequiresX25519MLKEM768`, `TestReceivingTLSUsesMLDSA65AndInteroperatesWithLegacyEd25519` |
| rotate fences the old bearer | `TestReceivingTLSRefusesUnpinnedKeysBeforeAnyFrame` (revoked, expired, retired and reserved pins), `TestReceivingGatewayKeys` |
| sender, null sender, IDN, envelope limits; senders match KyPost's rule | `checkReceivingArgs` runs on the MX host itself: `TestCloudflareContinuousSendersMatchWorker` (`senders.json`, the same sender rule as Maddy `bind`), `TestHostedBindArguments` |
| sender blocks match Go (`blocks.json`); a block expires at `until` | blocks live in tenancy on the replicas: `TestReceivingMultiModeBlocksLiveInTenancy`, `TestNativeReceivingBlockedSender` |
| plus addressing; noPlus; refused full address | `TestNativeReceivingPlusAddressing`, `TestInactiveLegacyPlusAddressKeepsItsOwner`, `TestNativeReceivingMaskedStates`; across cells, MH10's plus routing |
| size caps (pilot: oversized refused) | Maddy `max_message_size 25M`, `TestNativeReceivingRefusesUnimportableMessage` |
| table signature, ordering, staleness, etag cache, paging, canonical keys | no table on the MX host |
| workerd runtime checks (`runtime-*.mjs`) | `TestNativeReceivingMaddyRuntime` against the pinned Maddy (CI's `direct` and `configured-limits`) |

### New in the build

- **Public hop is hybrid.** `TestNativeReceivingMaddyRuntime` checks every launched mode:
  - STARTTLS negotiates `X25519MLKEM768` for a default Go client;
  - a P-256-only client still connects;
  - the bare binary (`direct`) negotiates X25519. That proves the launcher is what turns the
    hybrid on.
- **Memory cap.** `scripts/check-replicas-compose.py` requires the MX service's `mem_limit` and
  `memswap_limit` to be 768 MiB.

## Out of scope and follow-ups

- **ky-kubernetes** MX manifests (W6).
- **PQ row 41**, the Cloudflare continuous signatures: revisit when workerd ships ML-DSA. Re-run
  the probe after each pinned workerd bump.
- Maddy's `DefaultGODEBUG` also has `tlssha1=1`, which allows SHA-1 signatures in TLS 1.2
  handshakes. Turning that off may refuse old senders; it is left as it is.
- Relay code allocation and the e2e cell mapping stay with the owner (MH10).

## As built

- `scripts/start-receiving.sh` appends `tlsmlkem=1` to `GODEBUG` before it starts Maddy. An operator's
  setting (the strong-key overlay's `fips140=on`) still reaches Maddy and the helpers. That covers
  the single container and the MX host: Maddy and the helpers it runs. The helpers are Go 1.27,
  where this is already the default.
- `TestNativeReceivingMaddyRuntime` checks the negotiated group:
  - `X25519MLKEM768` in every launched mode (CI runs `configured-limits`);
  - X25519 for the bare binary;
  - P-256 for a classical-only sender.
  - every helper Maddy runs sees the operator's `GODEBUG` with `tlsmlkem=1` appended.
  - Mutations: without the export, `configured-limits` and `launcher` fail with "negotiated
    X25519, want X25519MLKEM768"; an export that replaces `GODEBUG` fails the helper check.
- `scripts/check-pqc-tls.sh` still refuses every `tlsmlkem` setting except `tlsmlkem=1` on a
  line marked `pqc-tls:allow`, and a marked line that also holds another value is refused.
- `docker-compose.mx.yml`: `mem_limit: 768m` and `memswap_limit: 768m`.
  `scripts/check-replicas-compose.py` requires both. Mutation: without `mem_limit`, the check
  fails.
- **This single-tenant change is a decision taken:** the public SMTP hop of a single-container
  install now offers the hybrid too. It only adds a group. Senders that do not offer the hybrid
  negotiate as before, and the generated Maddy profile is byte-identical (golden tests
  unchanged).
- **Owner rule (28 Nov), checked:** public images must never ship multi-tenant mode on. This
  changes no startup gate.
