# KyPost hosted receiving tier, design

Date: 2026-11-21. Status: proposed design; PR 1 (#686), PR 2 (TLS transport, #689), PR 3
(sender blocks and evidence in PostgreSQL, #691), PR 4 (import leases, #694) and PR 5 (hosted
profile) built.

This is the receiving tier's own spec, which the owner asked for on 2026-11-21 (answer to open
question 3 of KyPost's tenancy design, `docs/superpowers/specs/2026-11-20-tenancy-design.md`,
cited as **K**). It expands K's "Recommended hosted receiving tier" into a buildable design. It
also covers the masked relay domain (M open question 5 in
`docs/superpowers/specs/2026-11-19-masked-addresses-hosted-design.md`, cited as **M**) and K open
question 8 (capacity). It follows K's fixed owner decisions: one binary, single-tenant unchanged
byte for byte, one org per account, staff-only administrators, and one shared relay domain.

Line references are to KyPost `origin/main` at `67756dc2`. Paths are relative to
`backend/internal/` unless they start with a top-level folder (`docs/`, `scripts/`,
`receiving-worker/`) or name a root file. "Unverified" marks a claim this note did not check in
code, in a primary source or by running it. Nothing here was run; every statement about behaviour
comes from reading code.

## Summary

- Hosted KyPost receives mail on **two or more stateless MX hosts** that run today's pinned Maddy
  0.9.5 and today's `kypost-server receiving bind|accept` helpers. The helpers send their checked
  command to `receiving serve` on the KyPost replicas over **mutually authenticated TLS** on a
  private network, instead of over `receiver.sock`. The checks, their order, the exit codes and
  the SMTP answers stay as they are.
- An MX host answers `250` only after `accept` has **committed to the PostgreSQL ingress store**.
  An MX host holds nothing: losing one loses no accepted mail.
- **Rspamd runs beside each replica**, where `accept` already calls it, so an MX host cannot skip
  or forge the verdict. Sender blocks and sender evidence move from JSON files into PostgreSQL
  tables in multi mode.
- **Import is leased per mailbox**, so every replica imports, instead of the one `daemon` replica.
- The **masked relay domain** is served by the same MX hosts. No third party sees the list of
  masked addresses.
- **Cloudflare stays a self-hosted profile.** Its documented limits (checked below) and its
  visibility into every address rule it out for hosted mail. KyPost's 5000-route cap is KyPost's
  own number. No Cloudflare limit motivates it.
- **Single-tenant does not change.** Self-hosted profiles, the socket, the JSON stores and the
  daemon importer stay exactly as they are. Every new piece is refused unless
  `KYPOST_TENANCY=multi`.

## Today's receiving paths

One profile per deployment, chosen in setup (root `AGENTS.md` "Mail sources";
`docs/guides/NATIVE_MAIL.md:22-37`).

### Maddy on port 25 (self-hosted, single container)

- **Engine.** The operator mounts the qualified Maddy 0.9.5 binary. `scripts/start-receiving.sh`
  refuses any other hash (`:17-18`), regenerates the configuration through `kypost-server
  receiving config` (`:26-30`) and runs Maddy under `ulimit -n 256` (`:33-34`). No engine is
  bundled, and `KYPOST_NATIVE_RECEIVER=false` by default (`Dockerfile:175`).
- **Generated configuration** (`app/receiving_config.go:80-217`). The listener requires TLS
  from the sender (`require_tls`, `:191`) and caps a message at 25M and 100 recipients. It applies
  four connection limits from settings (`:184-189`; defaults are 4 transactions in all, 2 per IP,
  30 a minute in all and 10 per IP, `docs/RECEIVING_SETUP.md:296-297`). It runs two `check`
  commands (`:192-211`):
  - `receiving bind {msg_id} {sender} {address}` on each `RCPT`;
  - `receiving accept {msg_id} {sender}` on the body. With Rspamd it also passes
    `{source_ip} {source_host}` (`:42-45`).
  Mail that passes goes to `deliver_to dummy` (`:213`): once the checks pass, Maddy keeps nothing.
  The KyPost commit is the delivery.
- **Exit codes to SMTP answers** (`app/receiving_config.go:192-211`):

  | Exit | `bind` (RCPT) | `accept` (DATA) | Raised by |
  |---|---|---|---|
  | 0 | accept recipient | `250` | success |
  | 1 | `451 4.3.0` storage unavailable | `451 4.3.0` storage unavailable | any other error, `ingress.ErrCapacity`, a lost answer from `receiving serve` (`app/receiving.go:199-211`, `app/receiving_serve.go:71`) |
  | 3 | `550 5.1.1` recipient unavailable | `451 4.3.0` routing unavailable | `ingress.ErrRoute` |
  | 4 | — | `550 5.7.1` spam policy | Rspamd `reject` (`app/receiving_spam.go:110-111`) |
  | 5 | — | `451 4.7.0` spam check deferred | Rspamd unavailable, `soft reject` or `greylist`, or a sender or peer mismatch (`app/receiving_spam.go:48,112-113`, `app/receiving.go:621-624`) |
  | 6 | `550 5.7.1` sender blocked | — | `app/receiving.go:412-414` |
  | 7 | `550 5.1.7` sender not accepted | — | a sender either profile refuses (`app/receiving.go:405-407`, `:140-146`) |
  | 8 | `451 4.3.0` sender blocks unreadable | — | `app/receiving.go:408-411` |
  | 9 | `452 4.2.2` mailbox full | `452 4.2.2` mailbox full | `app/receiving.go:79` |
  | 10 | `452 4.3.1` system storage | `452 4.3.1` system storage | `fsutil.ErrDriveReserve` |
  | 11 | — | `554 5.6.0` headers cannot be stored | `app/receiving.go:44` |

  What Maddy answers for an exit code the table does not list (for example 6 from `accept`) is
  unverified. No code path above raises one.

  The hosted MX configuration adds two `bind` codes. Single-tenant configurations never contain
  them, and codes 0–11 do not change:

  | Exit | `bind` (RCPT) | Raised by |
  |---|---|---|
  | 12 | `451 4.2.1` mailbox temporarily unavailable | K's org check: the owner's org is `suspended`, or missing from the `orgs` cache (K "Inbound routing to the right org"). `deletion_scheduled` and `deleted` use exit 3, the unknown-address `550 5.1.1`. |
  | 13 | `450 4.7.1` try again later | The relay domain's per-source probe budget (below) |
- **`bind`** (`app/receiving.go:404-483`), in order: sender shape (exit 7), sender blocks (8,
  6), then `bindExpected`: fresh DNS proof of the recipient domain (`verifyDomains`), the plus base
  (`NativeAddressFor`), the assignment, the active address, the owner's refusals (plus tag,
  paused or expired masked address), and then, under the authority fence, the admitted owner,
  room for one more largest message, the route refresh and `holding.Bind`. K adds the org-status
  check after the assignment (K "Inbound routing to the right org").
- **`accept`** (`app/receiving.go:592-644`) reads at most 25 MiB within a 30-second budget, runs
  Rspamd on a staged delivery, then `commitAccept` (`:646-663`). Under the frozen owners' current
  authority and each mailbox's quota lock, that function checks the per-message limit,
  importability and room, then calls `holding.Accept`. An archived delivery answers an exact
  replay and writes nothing (`:610-613`).
- **`receiving serve`** (`app/receiving_serve.go`), an optional accelerator under supervisord
  (`supervisord.conf:96-105`), keeps one runtime warm. A helper sends one JSON line (`v`, `args`,
  an absolute `deadline`, `length`) and then the message (`:38-46`, `:55-95`). The server answers
  one exit-code byte (`:172-218`). It checks the peer's UID with `SO_PEERCRED` (`:235-246`) and
  runs at most 32 commands at once (`:34`). With no server listening, the helper runs the command
  in process (`:66-69`). Once a request is sent, a lost answer is exit 1 and is never retried
  locally (`:52-54`).
- **The ingress store** is `ingress.db`, or the `kypost_ingress` schema on PostgreSQL
  (`ingress/pg_schema.go:13-19`). It holds `routes`, `deliveries` (primary key `(gateway, id)`,
  with a `lease` and `lease_until`), `bindings` and `archived` tombstones. Its install-wide limits
  are 25 MiB a message, 512 MiB held and 10,000 records (`ingress/store.go:42`). They are only
  ever raised (`ingress/AGENTS.md`). Maddy's gateway ID is the constant `maddy-local`
  (`app/receiving.go:29`).

### Import by the `daemon` role

`startReceivingImport` (`app/receiving.go:214-262`) runs on the replica that holds the `daemon`
role (`worker/worker.go:13`, `app/app.go:371-382`; `docs/RUNNING.md:271`). Every 5 seconds it
lists each of the two fixed gateways, `maddy-local` and `cloudflare-worker` (`:230`), 100 rows at
a time, and imports each pending delivery in sequence, one by one. `importFrozen` (`:731-781`)
opens each bound mailbox, takes the shared authority fence and each mailbox's quota lock, and
commits every mailbox receipt before the acknowledgement. The per-delivery lease (`ingress`
`claim`, `ingress/store.go:830-884`) fences a second importer. Pending mail whose binding the
ledger durably retired is quarantined (`:712-729`).

### Sender blocks and evidence

- **Manual and automatic blocks** are one JSON file, `sender-blocks.json`, beside `ingress.db`
  (`ingress/blocks.go:22-23`). `bind` reads the whole file on every `RCPT`
  (`app/receiving.go:408`).
- **Automatic evidence** is `sender-evidence.json` (`ingress/autoblocks.go:26`), capped at 4096
  records of each kind and 8 MiB (`:44-54`). Each accepted or spam-rejected message with
  authentication calls `senderEvidence` (`app/receiving_blocks.go:145`), which rewrites the file.
  Automatic blocks need envelope sender = From, aligned DKIM and SPF from Rspamd's own result
  (`app/receiving_spam.go:118-126`). Thresholds: 5 in an hour, cooling down for 1 hour, then 24
  hours, then 7 days (`ingress/autoblocks.go:36-57`; `docs/FEATURES.md:27`).

### Rspamd

`accept` calls a fixed `http://127.0.0.1:11333/checkv2` (`app/receiving_spam.go:21`) with an
8-second budget (`:54`). The sidecar shares the KyPost container's network namespace
(`docker-compose.rspamd.yml:13`). It is stateless: no Redis, no Bayes, and no remote fuzzy
service (`scripts/rspamd/rspamd.conf:1-2`). The Cloudflare profile requires Rspamd
(`app/receiving.go:597-599`).

### Cloudflare continuous

The `daemon` replica signs and publishes a routing table of every admitted address
(`buildRoutes`, `app/receiving_cfcontinuous.go:342`). It picks mail up from the Worker's private
R2 queue over outbound HTTPS every 30 seconds (`pickup`, `:446`; `docs/FEATURES.md:12`), binds
each message to the owner frozen when the Worker accepted it (`capture`, `:582`), scans it,
commits it, and then deletes the provider copy (`deleteCommitted`, `:722`). The Worker sits behind
the zone's catch-all rule, with no per-address rules (`docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md:74-76`).
Its table holds at most 5000 routes and 5000 blocks in a 1 MiB signed document
(`cfreceiving/wire.go:32-34,211`; `receiving-worker/continuous.mjs:6`). It has been tested
offline only (`docs/guides/NATIVE_MAIL.md:37-39`).

### What R's replicas allow today

R refuses Maddy on any PostgreSQL install (`replica/topology.go:33-34,62-64`; `app/replica.go:26-29`)
and pins replicas to one host (`replica/topology.go:31-32,80`). `docs/RUNNING.md:252-254` says the
Cloudflare profile works with replicas. However, `docker-compose.rspamd.yml:4-5` refuses the
Rspamd overlay when the replicas overlay is present, and Cloudflare pickup refuses to accept
without Rspamd (`app/receiving.go:597-599`). From reading the files, **a replica install has no
working native receiving profile through the shipped Compose files** unless the operator runs
Rspamd at `127.0.0.1:11333` in each daemon container by other means. This was not run. PR 1 below
corrects the doc or the overlay.

## Cloudflare Email Routing's real limits

Checked on 2026-11-20 against Cloudflare's own documentation only. The Email Routing pages now
live under "Email Service".

| Limit | Figure | Source |
|---|---|---|
| Routing rules per domain | 200 ("Each rule maps an email pattern to a destination"); the page offers a limit-increase form and does not say whether this limit can be raised | `https://developers.cloudflare.com/email-service/platform/limits/` |
| Destination addresses per account | 200 | same page |
| Inbound message size | 25 MiB ("Messages larger than this are rejected") | same page |
| Domains per zone (Routing and Sending together) | 30 | same page; `https://developers.cloudflare.com/email-service/configuration/domains/` |
| Catch-all | "Catch-all entries support apex domains only"; its action can be "Send to a Worker" | `https://developers.cloudflare.com/email-service/configuration/email-routing-addresses/` |
| Worker CPU | 10 ms (Free); up to 5 min, default 30 s (Paid). Listed for HTTP requests, and the email page says email Workers get the standard limits | `https://developers.cloudflare.com/workers/platform/limits/` |
| Worker memory | 128 MB per isolate | same page |
| Inbound message rate or volume | not found in the docs | — |
| When `setReject` answers on the wire (RCPT or after DATA) | not found in the docs. "Reject this email message by returning a permanent SMTP error back to the connecting client" | `https://developers.cloudflare.com/email-service/api/route-emails/email-handler/` |
| Privacy | The older Email Routing docs said "Cloudflare will not store or access the emails routed to your inbox". The new pages do not repeat it, and KyPost's Worker stores every message in R2 by design | `https://developers.cloudflare.com/email-routing/llms-full.txt` (older text) |

What this means:

- **The 5000 is KyPost's own.** No Cloudflare document names it. It sits beside KyPost's own
  1 MiB signed-table budget (`cfreceiving/wire.go:33-34`). From the `wireRoute` fields
  (`cfreceiving/wire.go:186-192`), a route for a 30-character address is about 75 bytes, so about
  14,000 such routes would fit in 1 MiB. A route for a 254-character address is about 300 bytes,
  so only about 3500 would fit. Both figures are estimates; nothing was measured. Raising the
  number is possible on Cloudflare's side. The real costs are the Worker's 128 MB isolate and the
  CPU it spends parsing and verifying the table (unverified, not measured).
- **The catch-all is apex-only.** A receiving domain on a subdomain, such as `relay.<platform>`
  or M's per-shard `s1.relay.<platform>`, cannot use the catch-all. Each of its addresses would
  need its own rule, at 200 per domain. KyPost's Worker design therefore needs an apex domain.
- **Visibility.** Every message goes through Cloudflare and is stored in R2, and the signed table
  lists every address (`docs/guides/NATIVE_MAIL.md:26`). For hosted mail that means one third
  party holds every customer's address list and mail.

Decision R1 below keeps Cloudflare for self-hosted installs and leaves it out of hosted ones.

## Decisions this note makes

| # | Decision | Why |
|---|---|---|
| R1 | Hosted (multi) installs receive only through the receiving tier below. Cloudflare continuous and external IMAP are refused in multi mode. | Cloudflare's visibility into every customer address, its apex-only catch-all, and K6 (no external IMAP). |
| R2 | MX hosts are stateless and untrusted for authority. They run pinned Maddy, the same `kypost-server` binary as the helper, and a TLS client key. They hold no mailbox data, no ledger and no KyPost secrets. | A lost or compromised MX host must not lose mail or widen access. |
| R3 | The `receiver.sock` handoff gains a TLS transport, `receiving serve --tls`, with mutual authentication by **pinned public keys** (SPKI SHA-256), not by a CA. The frame (header line, body, one exit-code byte) and every exit code stay as they are. | The smallest change to a working protocol. Pins need no CA, CRL or OCSP. |
| R4 | The **gateway ID comes from the MX host's pinned key**, never from the request. Each MX host is its own gateway in `deliveries(gateway, id)`. | `ingress/AGENTS.md` already requires gateway IDs from authenticated transport. Maddy `msg_id` values from different hosts can never collide, and one host cannot touch another's deliveries. |
| R5 | `250` only after the PostgreSQL commit, and PostgreSQL commits must survive a failover (synchronous replication when PostgreSQL becomes its own service). | An accepted message is owed. A `250` backed only by an asynchronous primary can be lost in a failover. |
| R6 | Rspamd runs **beside each replica**, as a sidecar sharing its network namespace, so the fixed `127.0.0.1:11333` stays. Not on the MX hosts. | The verdict and the evidence are computed where the commit happens. An MX host can feed in only the peer IP and HELO (Threat model). |
| R7 | In multi mode, sender blocks and evidence live in PostgreSQL tables in the multi-only `tenancy.db` store (Where the new tables live), with the same semantics. Single-tenant keeps the JSON files. | A whole-file rewrite per message does not scale. Hosts past one need shared state. Multi installs start empty (K2), so nothing is migrated. |
| R8 | Import is **one obligation per bound mailbox**, leased per mailbox in PostgreSQL, so every replica imports. No mailbox waits on another. | Spreads import across replicas and keeps each mailbox's arrival order. One recipient's org status, or a busy mailbox, cannot hold back another recipient's mail. |
| R9 | The masked relay domain is one more `destination` on the same tier (M open question 5). | No third party lists masked addresses. Routing is by address, so nothing else changes. |
| R10 | Everything above is refused unless `KYPOST_TENANCY=multi`. Single-tenant profiles, files and code paths are untouched. | Owner decision 1. |

## Architecture

```
 internet senders
   │  SMTP + STARTTLS (require_tls), MX records of equal preference
   ▼
 ┌────────────┐   ┌────────────┐      stateless MX hosts (R2)
 │ mx-a Maddy │   │ mx-b Maddy │      pinned Maddy 0.9.5 + kypost-server helper
 └─────┬──────┘   └─────┬──────┘
       │ mTLS, pinned keys (R3), private network only
       ▼                ▼
 ┌───────────────────────────────┐   KyPost replicas (R, one host in v1)
 │ receiving serve --tls  (each) │   bind/accept checks unchanged
 │ Rspamd sidecar         (each) │   R6
 │ import workers, per-mailbox   │   R8
 │ leases                        │
 └───────────────┬───────────────┘
                 ▼
         PostgreSQL: kypost_ingress (deliveries, bindings, archived; unchanged)
                     kypost_tenancy (multi-only: receiving_gateways,
                     sender_blocks, sender_evidence, import_leases,
                     retired_bindings)
```

### MX host

- **What runs.** Pinned Maddy, started by `scripts/start-receiving.sh` with the same hash check,
  plus `kypost-server` as the check helper. The helper runs `checkReceivingArgs` and
  `readReceivingMessage` locally, as today (`app/receiving.go:110-129`), then forwards the
  command over TLS.
- **Not a replica.** An MX host never opens PostgreSQL or the volumes, so R's start checks do not
  apply to it. Replicas keep refusing `KYPOST_NATIVE_RECEIVER` (`replica/topology.go:62-64`):
  Maddy never runs on a replica.
- **No local fallback.** Today a helper with no server runs the command in process
  (`app/receiving_serve.go:66-69`). On an MX host, `KYPOST_RECEIVING_REMOTE` is set and that
  fallback is removed: no server means exit 1, `451 4.3.0`. The MX host has no state to run
  against anyway; the new rule makes the refusal explicit.
- **Configuration.** The Maddy file is generated on a replica (`receiving config` already reads
  the domain set under its fence, `app/receiving_config.go:142-170`). The MX host fetches it over
  the same TLS channel with a new `config` command when it starts, and every 5 minutes after. It
  restarts Maddy only when the file changed. A stale `destination` list only refuses or admits a
  domain at `RCPT`, and `bind` re-proves the domain anyway (`verifyDomains`). So staleness can
  delay a new domain but cannot route mail wrongly.
- **Public TLS.** Each MX host has its own certificate for its own MX hostname, as
  `receiving config` requires today (`app/receiving_config.go:103-125`).
- **Limits.** Maddy's per-IP and overall limits stay per host (`app/receiving_config.go:184-189`),
  so N MX hosts allow N times the overall rate. That is intended: the per-host figures size each
  host.

### The authenticated call (bind/accept over mTLS)

- **Listener.** `receiving serve --tls <addr>` on each replica, bound only to the private
  network address. It is refused unless `KYPOST_TENANCY=multi`. It uses Go's `crypto/tls` (TLS
  1.3 only, no new dependency) with `RequireAnyClientCert` and a `VerifyPeerCertificate` that
  compares the leaf's SPKI SHA-256 with `receiving_gateways`. `SO_PEERCRED` (`sameUser`) does not
  apply to TCP; the pin replaces it.
- **Server identity.** The MX helper pins the replicas' server SPKI hashes from its own
  configuration file. No public CA and no hostname check are involved.
- **Frame.** Unchanged: one JSON header line, the body, `CloseWrite`, and one exit-code byte
  (`app/receiving_serve.go:73-94`, `:172-218`). The protocol version becomes 3 for one change:
  `deadline` becomes `budget_ms`, the time remaining, because hosts do not share a clock. The
  server still caps each command at 30 seconds (`:185-205`). Version 2 stays as it is on the Unix
  socket.
- **Gateway ID (R4).** The server sets the runtime's gateway to the pinned row's ID for the whole
  connection, as the import loop already scopes it (`app/receiving.go:231-232`). The IDs
  `maddy-local` and `cloudflare-worker` are reserved and cannot be enrolled.
- **Concurrency.** 32 handlers per replica stays (`app/receiving_serve.go:34`). A helper that
  cannot get a slot waits until its own budget runs out, then exits 1.
- **Endpoints.** The helper is given the replicas' private addresses. It shuffles them and dials
  each with a 1-second timeout, but only **before** sending: once the header is written, a lost
  answer is exit 1, as today (`:52-54`). No load balancer is needed.

### Keys: issuance, rotation, revocation

| Step | How |
|---|---|
| Issue | `kypost-server receiving gateway keygen` on the MX host writes an Ed25519 key (owner-only, `0600`) and a self-signed certificate, and prints the SPKI SHA-256. The private key never leaves the host. |
| Enrol | On a replica, `kypost-server receiving gateway add <id> <spki-sha256>` (operator CLI, like `receiving blocks`) writes a pin row, `receiving_gateways(id, spki_sha256, created_at, expires_at, revoked_at)`, keyed by `(id, spki_sha256)`. `expires_at` defaults to 90 days. A key whose pin was ever revoked or expired is never enrolled again, under any ID. A retired ID (below) takes no new pins. The command writes an audit line with the ID and the hash prefix. A staff admin route can come later (open question 2). |
| Rotate | Generate a new key on the host, enrol it under the **same** ID (at most two live pins per ID), switch the helper, then revoke the old pin. The overlap keeps mail flowing. A pin past `expires_at` is refused like a revoked one. Revocation and expiry act on that one pin, never on the ID. A host whose pins have all expired recovers the same way: it generates a new key and that key is enrolled under its own ID. The host's mail flows through the other MX hosts meanwhile. |
| Revoke | `receiving gateway revoke <id> <spki>` sets `revoked_at` on that pin. `receiving gateway retire <id>` revokes every pin of the ID and marks the ID retired, so it can never be enrolled again (for a decommissioned or compromised host). Every replica re-reads the table for each new connection (or caches it for at most 5 seconds), and connections last one command (at most 35 seconds), so a revocation takes effect within about 40 seconds. Neither command touches deliveries. Its staged rows wait for the fenced cleanup, and its committed mail still imports. |
| Server keys | All replicas share **one** listener key, created exclusively in the shared `SECRET_DIR` like R's other keys, so replicas starting together agree (`docs/RUNNING.md:282`). `receiving gateway server-pin` prints its SPKI, and MX helpers pin that current key and the next one. Rotation: create the next key and pin it on every MX host, switch the replicas to it, then remove the old pin. Past one host, the key is shared the way the other `SECRET_DIR` keys are (R's follow-up). |

### Replay, ordering, timeouts

- **Replay.** TLS 1.3 protects the channel itself against replay. A command repeated over the
  channel is harmless by today's design: an exact `accept` of an archived delivery succeeds and
  writes nothing (`app/receiving.go:610-613`). The archived tombstone refuses a new recipient,
  sender, bytes or digest (`ingress/AGENTS.md`). A stolen MX key can replay only within its own
  gateway ID (R4).
- **Ordering.** Maddy runs `bind` for each `RCPT` and then `accept` (`run_on rcpt`, `run_on body`).
  Two commands of one transaction may reach different replicas. That is safe because the ingress
  store is one PostgreSQL schema and the authority and quota fences are `flock`s on the shared
  volume (one host, `docs/RUNNING.md:240-245`). `accept` without a `bind` finds no staged
  delivery and is refused.
- **Timeouts.** The helper has 30 seconds per command (`app/receiving.go:114`), with a 1-second
  dial and 5 seconds of margin for the answer (`app/receiving_serve.go:66,72-73`). Rspamd gets 8
  seconds inside that (`app/receiving_spam.go:54`). Maddy's own `check` timeout and its
  `read_timeout 30s` (`app/receiving_config.go:180`) bound the session. Whether Maddy kills a
  check command that runs past its own deadline is unverified. PR 2 must test a hung replica.

### `250` after commit, and MX failover

- `accept` returns 0 only after `holding.Accept` has committed (`app/receiving.go:646-663`).
  Maddy then answers `250` and discards its copy (`deliver_to dummy`). On PostgreSQL that commit
  is a transaction commit. With `synchronous_commit=on` (PostgreSQL's default) it is on the
  primary's WAL. R5 adds: once PostgreSQL is its own replicated service, a synchronous standby
  must have it before the commit returns. Otherwise a failover can lose mail that already got
  `250`.
- **An MX host dies before `250`.** The sender's session drops. The sender retries, usually
  against another MX record (RFC 5321 §5.1 and §4.5.4.1 cover MX fallback and retry; what each sender does is unverified).
  The new attempt gets a new `msg_id`, so it is a new delivery. The dead attempt's row stays
  `staged`; staged rows never expire on their own (`ingress/AGENTS.md`), and the existing "explicit
  fenced cleanup" operating gate handles them.
- **A replica dies after commit but before the answer byte.** The helper exits 1 and the sender
  gets `451` and retries, so the message may be **stored twice**. That is today's behaviour on
  the socket (`app/receiving_serve.go:52-54`): at-least-once, never lost. Whether import
  de-duplicates by Message-ID is unverified.
- **Every replica or PostgreSQL down.** Every MX host answers `451`. Senders queue and retry for
  days (sender-dependent, unverified). No mail is lost, as with K's "Not in the cache" rule.

### Rspamd placement (R6)

| Option | Verdict |
|---|---|
| On each MX host | Rejected. The scan's result and evidence would come from the internet-facing host, and `accept` would have to trust a verdict sent over the wire. |
| Beside each replica, sharing its network namespace | **Chosen.** `127.0.0.1:11333` stays fixed (`app/receiving_spam.go:21`), so there is no new URL setting. Rspamd is stateless (`scripts/rspamd/rspamd.conf:1-2`), so N copies agree. The refusal at `docker-compose.rspamd.yml:4-5` becomes "single container or the hosted profile". |
| A shared Rspamd pool on the private network | Rejected for v1. It needs a configurable URL and a separately authenticated hop. |

The MX host supplies `{source_ip}` and `{source_host}`, so Rspamd's SPF result is only as true as
the MX host. Automatic blocks also need aligned DKIM (`app/receiving_spam.go:118-126`), which an
MX host cannot forge, so a compromised MX host can only replay real signed mail with a false IP.
That residual risk is listed below.

### Where the new tables live

Every new table goes in the multi-only `STATE_DIR/tenancy.db` (PostgreSQL schema `kypost_tenancy`,
`tenancy/tenancy.go`). These are `receiving_gateways`, `sender_blocks`, `sender_evidence`,
`import_leases` and `retired_bindings`. None goes into `kypost_ingress`, and no index is added
there. K's "PR 1 as built" records why: existing stores' exact-schema checks and the PostgreSQL to
SQLite capsule copy refuse an extra table. A single-tenant install never creates `tenancy.db`.

Completion reads a binding's done-state from other stores (the mailbox's `receipts` and
`tenancy.db`) inside a `kypost_ingress` transaction. These are plain reads of committed rows, so no
transaction spans two stores.

**Backups.** `tenancy.db` is not in the backup set yet (K "PR 1 as built"). Sender blocks and
gateway pins must survive a restore, so this spec's PR 3 depends on K PR 6 adding it, or adds it
first itself.

### Sender blocks and evidence in PostgreSQL (R7)

- **Tables** (multi-only, in `tenancy.db`, class P in K's inventory):
  `sender_blocks(id PK, kind, value, until, source, level, created_at, actor, reason)` holds the
  fields of `ingress.SenderBlock` (`ingress/blocks.go:47-57`). `sender_evidence` holds one row per
  address, domain, good-domain and unblock record, with the caps of `ingress/autoblocks.go:44-46`.
- **Same rules.** The same normalisation and matching (`ingress.NormalizeBlock`, and the
  `receiving-worker/blocks.json` fixture still applies), the same "automatic uses at most half"
  and "manual wins" rules, and the same 30-day unblock suppression. The file and table
  implementations sit behind one interface, so `bind` does not branch on the mode.
- **Reads.** `bind` reads one indexed row per sender and domain, not the whole file.
- **Evidence writes.** One upsert per authenticated message, in its own transaction after the
  commit, as today (`app/receiving.go:640-642`). A failed evidence write never changes whether
  mail is accepted (`app/receiving_spam.go:126`).

### Import leased per mailbox (R8)

- **Obligations per mailbox.** In multi mode a delivery is one import **obligation per bound
  mailbox**, not one all-or-nothing import. An obligation is done when the mailbox holds the
  receipt (its `receipts` table is keyed by `(gateway, delivery)`, `mailbox/store.go:127`), or
  when the binding is recorded in the multi-only `retired_bindings(gateway, id, mailbox, at)`.
  Mailboxes are therefore independent. A delivery bound to A (active org) and B (an org deleted
  but not yet purged) imports into A at once, and only B's obligation waits for the purge. No mailbox's import waits on another
  mailbox's import, and no worker holds more than one lease (Completion has one short row lock).
- **Leases.** `import_leases(mailbox PK, holder, lease_until)`, multi-only. Every replica runs
  import workers, not only the `daemon` replica. A worker takes a 60-second lease on one mailbox
  with outstanding obligations (`UPDATE … WHERE lease_until < now() … SKIP LOCKED`). It renews the
  lease while it works and gives it up after one batch: at most 100 obligations or 60 seconds,
  even when more are pending. No mailbox stays leased continuously.
- **Listing a mailbox's obligations** scans `bindings` by `mailbox`. No index is added, so
  `kypost_ingress` keeps its schema. Bindings exist only for deliveries that are not yet
  archived, and the 10,000-record ingress limit (`ingress/store.go:42`) bounds them, so the scan
  stays small. The capacity measurement checks it.
- **One obligation.** In arrival order, for each pending delivery bound to the leased mailbox
  whose obligation is not done, the worker:
  1. checks that mailbox's org status (Org status, below). For a `deleted` org it stops, and the
     purge removes the obligation. For an org missing from the cache it stops that mailbox's queue
     until the next good poll. Otherwise it continues;
  2. if the binding was durably retired (today's `retiredBinding` test,
     `app/receiving_cfcontinuous.go:788`), inserts its `retired_bindings` row; otherwise writes
     the message into that mailbox under its quota lock, which commits the receipt;
  3. runs Completion.

  Step 2 is idempotent. A repeated write finds the receipt, and a repeated retired row hits its
  primary key. The mailbox's order is its own arrival order, and nothing in another mailbox can
  block it.

  **Fence against the purge.** The mailbox write in step 2 re-reads the org's status from the
  `orgs` cache **after** taking that mailbox's quota lock (`quota-<id>`, `app/receiving.go:54-66`),
  and writes only if the org is not `deleted`. The purge (K "Purge", step 3) marks the org
  `deleted` in the cache before it starts. It then takes the same quota lock for each mailbox
  before removing its storage, and holds the lock until the removal commits. So either the write
  finishes before the removal starts, and the removal deletes it, or the write sees `deleted` and
  writes nothing. A write can never land in or recreate storage the purge has removed. The purge's
  final check (K step 6) also lists receipts for the snapshot's mailboxes.
- **Completion.** After its own step 2 has committed, the worker runs one short
  `kypost_ingress` transaction:
  1. lock the delivery's row (`SELECT … FROM deliveries … FOR UPDATE`);
  2. check every binding: done if its mailbox holds the receipt or `retired_bindings` has it;
  3. if all are done, archive the delivery (`Acknowledge`'s tombstone write) when none is
     retired, or quarantine it, keeping only its retired bindings, when any is.

  Every done-record is committed **before** its writer takes the lock. So of two workers
  finishing A and B at once, the second to get the lock sees both and completes. The lock never
  covers a mailbox write: a worker waits on another mailbox only for that short transaction
  (milliseconds), never for its import. Quarantine and purge change a delivery's state under the
  same row lock. A backstop sweep on the elected `api-sweepers` worker runs Completion for any
  pending delivery, for example after a crash between step 2 and Completion. The whole-delivery
  claim lease of today's importer (`ingress/store.go:830-884`) is not used for multi-mode
  obligations.
- **Retired bindings in quarantine.** The quarantined delivery holds only its retired bindings
  and appears in staff quarantine as today. Release and discard act on those bindings. Release
  already resumes through the receipts (`ingress/import.go:30-33`), so a mailbox that already has
  the message is a no-op.
- **Purge.** K's purge step 2 deletes the org's bindings and their `retired_bindings` rows, and
  then runs Completion: archived if the rest are imported, quarantined if any is retired, still
  pending if some are not done yet.
- **Held bytes.** A delivery's bytes stay in the shared ingress store until every obligation is
  done or purged, and every org shares the ingress limits. Import therefore never holds mail for
  an org's status alone:
  - a `suspended` or `deletion_scheduled` org's accepted mail imports, because K keeps that data;
  - a `deleted` org's mail is removed by the purge, in minutes;
  - only a mailbox whose org is missing from the cache waits.

  So held mail is bounded in time by the poller and purge, not by billing. A cache outage holds at
  most what arrived for unknown orgs, and `bind` already refuses those (exit 12). Hosted ingress
  limits must exceed the backlog of one poller outage at peak rate. The capacity measurement sizes
  this (open question 6).
- **Gateways.** The import list is every gateway ID that has pending deliveries
  (`SELECT DISTINCT gateway FROM deliveries WHERE state='pending'`), not the two fixed IDs of
  `app/receiving.go:230`, and never the live rows of `receiving_gateways`. A revoked or expired
  pin stops new commands only. Mail its host already committed has had its `250` and still
  imports. Pin rows are never deleted, so a revoked key and a retired ID stay refused.
- **Org status.** Applied per mailbox, never to a delivery as a whole.
  - `active`, `suspended`, `deletion_scheduled`: import. K keeps those orgs' data, and storing
    mail that was accepted before the status changed serves it to nobody, because sign-in is
    refused.
  - `deleted`: no import. The purge drops the obligation.
  - Missing from the cache: wait for a good poll.

  This narrows K's "a worker skips a mailbox whose org is not `active`" for receiving import
  only (K "Background workers" is updated to match). It stops one org's suspension from holding
  shared ingress capacity for every other org.
- **Single-tenant.** The daemon importer of `app/receiving.go:214-262` stays exactly as it is.

### Past one host

R pins replicas to one host because the authority, users and domain records, and the quota locks
(`app/receiving.go:54-66`, `:369`), are `flock`s on shared volumes (`docs/RUNNING.md:240-245`).
The receiving tier needs these to go past one host:

1. **R's follow-up.** Accounts, per-user files, the native authority and domain records move into
   PostgreSQL, with PostgreSQL locks in place of `flock`. That includes `quota-<id>` (an advisory
   lock per mailbox) and the domain fence. K already makes this a hosted launch prerequisite
   (K "Replicas").
2. **Sender blocks and evidence in PostgreSQL** (R7).
3. **PostgreSQL as its own replicated service**, with a synchronous standby (R5) and connection
   pooling sized for N replicas, each with 32 receiving handlers and its import workers.
4. **R's host check.** `replica.ErrHost` (`replica/topology.go:31-32,80`) is lifted only once 1–3
   have shipped.

MX hosts are already off-host in v1, because they hold no state. Only the replicas wait on R's
follow-up.

### The masked relay domain (M open question 5)

- The relay domain is one more proved platform domain (K9). It appears in the generated
  `destination` list (`app/receiving_config.go:213`) and resolves per `RCPT` like any other domain.
  Its MX records point at the MX hosts. Nothing is published to a third party.
- **RCPT probing.** M asks for a refused-recipient budget per source. `bind` gets no peer IP
  today (`app/receiving_config.go:192`). In the hosted configuration only, `bind` also receives
  `{source_ip}`.
  - The replica counts refused recipients **on the relay domain only**, per single source IP
    (IPv4 /32, IPv6 /64), in R's PostgreSQL windows.
  - Past N in a window, every further relay-domain `RCPT` from that IP, live or not, gets exit 13
    (`450 4.7.1`), so the answer reveals nothing.
  - Primary domains are not budgeted. Their addresses are not secret the way masked ones are.
  - **Ceiling.** A large provider can send from one IP for many senders, and deleted masked
    addresses keep receiving list mail. Such an IP can trip the budget, and then its relay-domain
    mail is temp-failed for the block period. Senders retry, so nothing is lost, but delivery is
    delayed for every customer behind that IP. SPF and DKIM cannot exempt it, because neither is
    known at `RCPT`.
  - N, the window and the block period are open question 5, and the capacity measurement counts
    how often real provider IPs would trip it.
- **Apex.** If the relay domain is ever moved to Cloudflare, it must be an apex domain, because
  the catch-all is apex-only.

### Outbound at hosted volume

Receiving does not change outbound, but the tier is sized with it in mind:

- **Today.** One relay profile per install (`mailmsg/domain_relay.go:26-42`). Request-path sends
  go out at once. The recovery worker on the `mail-runtime` replica walks every mailbox every 30
  seconds with 4 at a time (`app/native_outbound.go:31,45-51`). At hosted scale that walk grows
  with the number of mailboxes, not with the mail sent. Recovery should be driven by an index of
  queued outbox rows. That is a later change, sized by open question 8.
- **Recommended.** Relay mode through a sending provider with per-domain custom MAIL FROM, so
  SPF and DKIM align on the relay and primary domains (M "What the relay domain needs"). Direct
  delivery from warmed IPs is the alternative; it puts every customer's reputation on our own IPs.
- **Feedback.** A complaint and bounce feedback path (M open question 8) comes before hosted
  masked sending. Per-org send caps come from K's "Abuse limits per org".

## Single-tenant: unchanged

- The Maddy, Cloudflare and external IMAP profiles, `receiver.sock` (protocol 2, `SO_PEERCRED`,
  in-process fallback), `sender-blocks.json`, `sender-evidence.json`, the `daemon` importer, the
  fixed gateway list and the Rspamd overlay's single-container rule all stay exactly as they are.
- `receiving serve --tls`, `receiving gateway`, `KYPOST_RECEIVING_REMOTE`, the
  `receiving_gateways`, `import_leases`, `retired_bindings`, `sender_blocks` and `sender_evidence`
  tables (all in the multi-only `tenancy.db`) and exit codes 12 and 13 are each refused or never
  created unless
  `KYPOST_TENANCY=multi` is recorded (K1).
- **Proof.** K PR 1's single-tenant upgrade test, extended: a single-container Maddy fixture and
  an R replica fixture started under the new binary keep every `kypost_ingress` row and schema
  dump, `sender-blocks.json`, `sender-evidence.json` and the generated `receiving.conf`
  byte-for-byte unchanged. `TestEveryStoreIsClassified` gains the five new tables.
- Self-hosted R replicas with their own MX hosts would be possible later, but this spec gates the
  tier on multi mode (open question 1).

## Threat model

| Attacker | Goal | Defence |
|---|---|---|
| Internet sender | Deliver into the wrong mailbox or org | Unchanged routing by address on the replica (`bind`), with K's org check. The MX host decides nothing. |
| Internet sender | Probe for live addresses (primary or masked) | Identical `550 5.1.1` for unknown, paused, expired and deleted addresses (`app/receiving.go:448-457`). On the relay domain only, the per-source budget answers `450` to every `RCPT` after N refusals, whatever the address. Primary domains are not budgeted: their addresses are not secret (Masked relay domain). M's address entropy. |
| Internet sender | Exhaust receiving | Maddy limits per MX host. The ingress record and byte limits refuse with a temporary failure, never by evicting mail (`ingress/AGENTS.md`). Per-mailbox quota (exit 9). |
| Host on the private network without a pinned key | Inject or read mail | `RequireAnyClientCert` plus the pin check before any frame is read. The listener binds only to the private address. |
| Compromised MX host | Read mail in transit | It sees the mail it receives. That is inherent to running an MX and the reason it holds nothing else. It cannot read stored mail: it has no mailbox access, and the protocol has only `bind`, `accept` and `config`. |
| Compromised MX host | Forge or replay deliveries, or touch another host's | It can submit mail as a sender could, under its own gateway ID only (R4). It cannot acknowledge, release or import. Retiring its ID cuts it off within about 40 seconds. |
| Compromised MX host | Poison automatic blocks | It controls `{source_ip}` and HELO, so it controls the SPF input. Automatic blocks also need aligned DKIM, which it cannot forge, so it can only replay real signed mail. Residual risk. |
| Compromised MX host | Learn the address list | `config` returns domains only, never addresses. `bind` answers one recipient at a time, and its probes count against the source budget. |
| Stolen replica server key | Impersonate KyPost to MX hosts | MX hosts pin the server SPKI. Rotate by pinning the next key, then removing the old one. It is a `SECRET_DIR` key like the others (K "Keys"). |
| Operator or staff | Read customers' mail | As K: trusted. The tier adds no route that reads mail. |
| Anyone | Infer an org from SMTP answers | No answer names an org. Suspended orgs get `451 4.2.1` and deleted ones `550 5.1.1` (K). |

## Failure modes

| Failure | Sender sees | Mail |
|---|---|---|
| One MX host down | Tries the next MX record | Nothing lost |
| MX host dies mid-transaction | Session drop, then a retry | A staged orphan row, cleaned later |
| Replica unreachable before the request is sent | Nothing: the helper dials the next replica | — |
| Replica lost after the request is sent | `451 4.3.0` | Possibly stored twice, never lost |
| All replicas or PostgreSQL down | `451 4.3.0` from every MX host | Queued by senders |
| Rspamd down on a replica | `451 4.7.0` (exit 5, `app/receiving_spam.go:48`) | Retried |
| Import behind; the ingress limits reached | `451 4.3.0` (`ingress.ErrCapacity`, exit 1) | Retried. This is the main capacity risk (below). |
| Pin expired or revoked by mistake | That MX host answers `451` | The other MX hosts carry the mail |
| PostgreSQL failover with asynchronous replication | Nothing: the sender already got `250` | **Lost.** R5 forbids this setup. |
| Clock skew between hosts | Nothing, because `budget_ms` is relative | — |

## Capacity measurement (K open question 8)

Nothing has been measured. The defaults are conservative (`docs/RECEIVING_SETUP.md:294-323`
states they are not a capacity claim). The tier must be measured on R's one host before launch:

- **Inputs.** A load generator on its own host sends SMTP to two MX hosts, with a mix of message
  sizes: the median and 95th percentile of a real mail corpus, plus a fraction at 25 MiB.
  Recipients are spread over 10³, 10⁴ and 10⁵ synthetic mailboxes. Throwaway servers only, with
  `PUSH_RELAY_URL=` and `APNS_RELAY_URL=` empty and no real mail.
- **Measure.**
  - Accepted messages a minute at `250`.
  - p50 and p99 time from `RCPT` to `250`.
  - Temporary refusals by exit code.
  - Ingress depth (records and bytes) against the 10,000-record and 512 MiB limits.
  - Time from accept to import.
  - The `bind` cost of DNS proof (`verifyDomains` proves the domain on every `RCPT`).
  - PostgreSQL commit latency and schema count (K "Schema count").
  - Rspamd CPU per message.
  - Replica memory: about 50 MiB per active 25 MiB transaction (`docs/RECEIVING_SETUP.md:315-317`).
- **Expected limits to find.**
  - The install-wide ingress limits. 512 MiB held is about 20 messages at the 25 MiB cap, so import
    lag fills it quickly. Hosted mode will need higher limits; they can only ever be raised.
  - Per-RCPT DNS proof.
  - Whole-file sender-evidence rewrites, until R7 lands.
  - The outbox recovery walk.
- **Output.** A stated target (orgs, mailboxes, messages a minute, held bytes) recorded in this
  spec, and the measured settings behind it, as T owner answer 8 does for KyIdentity.

## Phased build plan

Each PR ships on its own. Single-tenant stays unchanged, and every PR runs the single-tenant
upgrade test. K's PR 7 depends on PRs 1–5. PR 6 is a launch prerequisite, not part of K's PR 7.

1. **Fix the replica-receiving contradiction.** The Rspamd overlay and `docs/RUNNING.md:252-254`
   must agree, as "What R's replicas allow today" shows. Either document that Cloudflare on
   replicas needs per-replica Rspamd and add it, or state that replicas have no native receiving.
   Docs and Compose only.
   *Built in #686:* replicas have no native receiving (this PR gave no recommendation).
   `docs/RUNNING.md` ("Replicas on one host") and both overlays now say so;
   `scripts/check-replicas-compose.py` fails if the old claim returns. The per-replica sidecar
   (R6) arrives with the hosted profile.
2. **TLS transport.** `receiving serve --tls`, protocol 3 (`budget_ms`), `receiving_gateways`, the
   `receiving gateway keygen|add|revoke|retire|server-pin` CLI, `KYPOST_RECEIVING_REMOTE` with no local
   fallback, the pinned gateway ID, and the `config` fetch. All refused outside multi mode. Tests:
   - wrong client key, revoked pin, expired pin, a retired ID and a reserved gateway ID, each refused before any
     frame is read;
   - wrong server key, refused by the helper;
   - replay of an archived `accept`, which succeeds and writes nothing;
   - a new recipient on an archived ID, refused;
   - an oversize body, and a `length` that does not match;
   - a hung replica, which gives exit 1 within the budget;
   - `bind` and `accept` landing on different replicas;
   - every exit code 0–11 mapping to the same SMTP answer as on the socket, and 12 and 13 present
     only in the hosted configuration;
   - single-tenant: `--tls` refused, protocol 2 unchanged.
   *PR 2 as built:* paths under `backend/internal/` at the PR's head. Startup still refuses
   `multi` (K PR 7), so the tests record multi mode directly, as K's do.
   - **Gates.** On a replica, `receiving serve --tls`, `gateway add|revoke|retire|server-pin`
     need `KYPOST_TENANCY=multi` **and** the install's multi record with the same staff org, then
     open `tenancy.db` (`receivingMultiStore`, `app/receiving_tls.go:75`). An MX host has no
     state, so `keygen` and `KYPOST_RECEIVING_REMOTE` need only `KYPOST_TENANCY=multi`
     (`receivingMultiEnv`, `:62`). Set outside multi mode, `KYPOST_RECEIVING_REMOTE` is exit 1,
     never the old path.
   - **`receiving_gateways`** (`tenancy/tenancy.go:45`, functions in `tenancy/gateways.go`).
     Readings: `spki_sha256` is `UNIQUE`, so a key ever enrolled (live, revoked or expired) is
     never enrolled again under any ID; a retired ID is a `retired_at` column on its pins, not a
     sixth table, so retiring an ID with no pins is refused. Triggers refuse a delete and any
     update except setting `revoked_at`/`retired_at` once. Enrolment is a serializable
     transaction, so concurrent `add`s keep at most two live pins. Gateway IDs are 1–63
     lowercase letters, digits and hyphens; `cloudflare-continuous` is reserved too.
   - **Keys.** `keygen <path>` writes only the Ed25519 key (PKCS #8 PEM, created exclusively,
     `0600`) and prints its pin. No certificate file: each side wraps its key in a self-signed
     certificate in memory (`receivingCertificate`, `:144`), because peers check only the SPKI
     pin. The replicas' key is `SECRET_DIR/receiving-tls.key`, created exclusively on first use
     (`receivingServerKey`, `:162`). Rotation of it is an operator file swap after pinning the
     next key on every MX host (`keygen` writes a key in the same format).
   - **Server** (`serveTLS`, `:314`). TLS 1.3, `RequireAnyClientCert`, and a
     `VerifyConnection` that reads the pin row per connection, so an unknown, revoked,
     expired or retired key and a reserved ID fail the handshake (2-second limit) and no
     frame is read. The runtime is copied per connection with the pin's gateway ID. A pin with
     less than 14 days left logs a warning on each connection; the staff console warning
     (open question 3) waits for K's PR 4/5 console. The listener takes only a literal
     private or loopback IP.
   - **Protocol 3** (`parseReceivingRequest`, `app/receiving_serve.go:241`): `budget_ms` 1–30000,
     unknown fields and trailing data refused, `config` only with no arguments and no body.
     Protocol 2 is parsed as before and is the only one on the socket.
   - **Helper** (`receivingRemote`, `app/receiving_tls.go:374`). `KYPOST_RECEIVING_REMOTE` names
     an owner-only JSON file: `key`, 1–16 private `servers`, 1–4 `server_pins`. It sends
     `budget_ms` = its deadline minus 5 s, so the replica stops first and a hung replica gives
     exit 1 inside the helper's own 30 s. Reading: a replica with no free slot leaves the
     handshake unanswered, so the helper moves to the next replica within 1 s instead of
     waiting out its budget; that is still before sending.
   - **`config`.** The replica answers the destination domains only, after `receiving config`'s
     own proof, fence and restore checks (`destinations`, `app/receiving_config.go:219`). The
     MX host's `receiving config <IP:port> <host> <cert> <key>` renders the profile locally
     around them, with its own paths, limits and certificate, always passing
     `{source_ip} {source_host}` to `accept`, and adding exit codes 12 and 13 to `bind`
     (`hostedBindCodes`, `:78`). The 5-minute refresh and Maddy restart belong to the MX host's
     start script, which is the hosted profile's (PR 5).
   - **Not in PR 2.** Nothing raises 12 or 13 yet (PR 5). The daemon importer still lists only
     the two fixed gateways, so mail an MX host commits waits for PR 4's pending-gateway list.
     `tenancy.db` is not yet in the backup set (above), so pins do not survive a restore yet.
     The upgrade test was extended to run the new commands on the single-tenant fixture
     (refused, nothing written), and a golden file pins the single-tenant `receiving.conf`;
     a single-container Maddy fixture and an R replica fixture were not added.
3. **Sender blocks and evidence in PostgreSQL** (R7), behind one interface. Tests: the
   `blocks.json` fixture, today's sender-block tests and `TestEvidenceWorstCaseSize`, all run
   against both implementations. Multi mode never touches the files.
   *PR 3 as built:* paths under `backend/internal/` at the PR's head.
   - **One interface.** `ingress.SenderStore` (`ingress/blocks.go:64`) is List, Blocked, Put,
     Remove, Reject, Accepted and Status. `FileSenders` (`:75`) is today's two files, unchanged;
     `TableSenders` (`ingress/senders_table.go:18`) is the tables. The rules are written once:
     `put` (`ingress/blocks.go:237`) over either home's read-modify-write, and `reject` and
     `accepted` (`ingress/autoblocks.go:359`, `:427`) over `evidenceState` (`:249`), which the
     file's document and the rows both implement. The file implementation keeps its bytes:
     maps are still created only when written.
   - **Selection.** `openReceivingRuntime` picks the tables when `KYPOST_TENANCY=multi`
     (`app/receiving.go:356-367`, through PR 2's `receivingMultiStore`); a replica without its
     multi record or `tenancy.db` refuses and never falls back to the files. The admin API's
     `senderBlocks` (`api/native_quarantine_handlers.go:163`) uses `Server.tenancy` the same
     way. `bind`, the CLI, `senderEvidence` and the Cloudflare publisher call the interface.
   - **Tables** (`tenancy/tenancy.go:56`, PostgreSQL `:92`). `sender_blocks` has the
     `SenderBlock` fields, primary key `id`. `sender_evidence(kind, id, ref, hits, level, last,
     blocked)`, primary key `(kind, id)`, has one row per address, domain, good domain and
     unblock. `ref` is an address's domain ID or a good domain's crude parent, `hits` a JSON
     array, and `last` is a good domain's first acceptance or an unblock's end. Reading:
     `parents` and `goodSince` are not stored; they are the count of good rows per `ref` and
     the earliest good row, which equals the file's value because the first acceptance is
     always recorded and good rows are never evicted.
   - **Reads and writes.** `Blocked` reads the sender's address row and its domain's row by
     primary key (`ingress/senders_table.go:71`). `List` and each block write read the whole
     table under the file's whole-list rule. Each write is one `tenancy.db` transaction, which
     the store serializes (SQLite's single writer, the PostgreSQL schema's writer lock), and
     writes only changed rows (`changeTx`, `:155`). An automatic block that `Reject` makes is
     written inside the evidence transaction (`:325`), so the two cannot disagree; a failed
     row write fails the whole transaction. Evidence cleanup, caps and eviction order are the
     file's, in SQL (`:206`).
   - **Differences from the files, by design.** A row that breaks the entry rule refuses the
     read that meets it (`Blocked` reads two rows, so a bad row elsewhere does not refuse
     `RCPT`). There is no damaged file to set aside, so `Status.ResetAt` stays null and
     `Damaged` means the table is unreadable. Writes need `tenancy.db`, not the receiving
     directory, so the list can be filled before `receiving init`.
   - **Tests.** `eachHome` (`ingress/senders_test.go:29`) runs `TestSenderBlocks`,
     `TestSenderBlocksMatchWorker` (the `blocks.json` fixture), every `TestAutomatic*` and
     `TestEvidenceWorstCaseSize` against both homes, and fails if the table home writes the
     receiving directory. File-only parts (file mode, `ParseBlocks`, `ParseEvidence`, damage
     reset, a missing directory) run only for the files. The table worst case seeds every
     kind at its cap with the widest values, writes once more, and checks every cap holds and
     the stored bytes stay under three quarters of `MaxEvidenceBytes`.
     `TestReceivingMultiModeBlocksLiveInTenancy` (`app/receiving_blocks_multi_test.go:20`) turns
     both file names into unreadable directories, then adds, lists and removes a block through
     the CLI and binds over TLS (exit 6, then 0); the directories are untouched and the rows are
     in `tenancy.db`. `TestReceivingMultiModeWithoutTenancyRefuses` (`:77`) covers multi mode
     without its record. `TestStaffSenderBlocksUseTenancyTables`
     (`api/tenancy_blocks_test.go:16`) does the same through the staff console's routes. In
     production `Server.tenancy` is set only once multi mode can start (K PR 7), so today the
     API always reads the files. `TestSingleTenantUpgradeIsByteForByte` now seeds both files and
     keeps them byte for byte. `TestEveryStoreIsClassified` has both tables (P), and the three
     sender files are absent (`-`) in multi mode.
   - **Not in PR 3.** `tenancy.db` is still outside the backup set (K PR 6; Where the new
     tables live), so multi-mode blocks and evidence do not survive a restore yet. Multi mode
     still cannot start (K PR 7). Cloudflare's change mark (`cfChangeMark`) still watches the
     file; Cloudflare is single-mode only by R1, and no code refuses it in multi mode yet
     (which PR adds the refusal is unverified). The staff console warning for a
     pin with 14 days or less left (open question 3) is not built: K PR 4 built no console
     screen, and the only receiving status screen is Cloudflare's, so it needs a new platform
     admin route and a banner, in the admin route tests K PR 5 is changing.
4. **Per-mailbox import obligations and leases** (R8), and the pending-gateway import list. Tests:
   - commit through a gateway, revoke its pin (and separately, let it expire), and the mail still
     imports;
   - revoking one pin leaves the ID's other pin working, and a second rotation under the same ID
     is accepted;
   - a host whose pins have all expired is re-enrolled under its own ID with a new key, and its
     old key is refused;
   - enrolling a retired ID, or any key that was revoked, is refused;
   - two replicas importing one mailbox keep arrival order and import nothing twice;
   - a delivery bound to mailboxes in two orgs, one deleted before import: it imports into the
     active org's mailbox, and that mailbox's later mail imports too. The deleted org's obligation
     stays pending only until its purge, and then the delivery is archived;
   - a suspended org's accepted mail imports and is archived, so the ingress limits are freed;
   - an org deleted between a worker's first status check and its write: the write under the
     quota lock sees `deleted` and writes nothing, or it finishes first and the purge removes it.
     Either way the purge's final check finds no mailbox, receipt or schema;
   - an org missing from the cache: its mailbox waits, and the other mailboxes import;
   - a worker killed between the mailbox write and Completion: the retry adds no second copy, and
     the sweep completes the delivery;
   - two workers finishing the last two obligations of one delivery at the same moment: it is
     archived exactly once, and the backstop sweep archives one whose last commit was followed by
     a crash;
   - a delivery bound to an active and a retired mailbox: the active one imports, the retired
     binding is recorded `retired`, and the delivery is quarantined with only that binding and
     listed in staff quarantine (not left pending);
   - a purge that leaves only `retired` rows quarantines, and one that leaves only `imported`
     rows archives;
   - a lease holder killed mid-batch, and a mailbox under constant load giving its lease up after
     each batch;
   - single-tenant: the daemon importer and all-or-nothing import are unchanged.
   *PR 4 as built:* paths under `backend/internal/` at the PR's head. Startup still refuses
   `multi` (K PR 7), so the tests set the runtime's `tenancy.db` directly, as K's do.
   - **Tables** (`tenancy/tenancy.go:67-68`, PostgreSQL `:105-106`; functions in
     `tenancy/imports.go`). `import_leases(mailbox PK, holder, lease_until)` and
     `retired_bindings(gateway, id, mailbox, at)`, primary key `(gateway, id, mailbox)`. Both are
     class D in K's inventory (the mailbox's org); the purge deletes them with the bindings.
   - **Leases** (`TakeImportLease`, `tenancy/imports.go:23`). One upsert that takes a free or
     expired lease, or renews the holder's own, for 60 s. Reading: no `SKIP LOCKED`; every
     `tenancy.db` write already runs under the store's single writer (SQLite's, or the
     PostgreSQL schema's writer lock), so two takers cannot both win. Release deletes only the
     holder's own row.
   - **Workers** (`startLeasedImport`, `app/receiving_imports.go:39`). Four per daemon process,
     started in `runDaemon` outside the role election (`app/app.go:375`), so every replica
     imports. Each pass lists the mailboxes with pending mail (`PendingMailboxes`,
     `ingress/obligations.go:36`) and runs one batch per mailbox (`importMailbox`, `:117`): take
     the lease, renew it before each obligation, attempt at most 100 obligations or 60 s in
     arrival order, then release it. A mailbox whose batch hit the bound is skipped for that
     worker's next pass, so another worker takes it. Readings:
     - **Arrival order** is the delivery's `rowid`, its first bind (`Obligations`, `:59`), across
       every gateway. An obligation already done (receipt or retired row) is skipped and does
       not count against the 100, so done mail waiting on another mailbox never starves later
       mail.
     - **A failed obligation** is logged and retried next pass, as the single-mode importer
       does; it does not stop the mailbox, so one bad delivery cannot block a mailbox for good.
       Only the org check stops the mailbox's batch (deleted, or not cached).
     - **The brief's "lease sets in sorted order, waiting"** is the draft RX replaced before
       #680 merged. This follows the merged text: one obligation per mailbox, one lease per
       worker.
   - **One obligation** (`importObligation`, `app/receiving_imports.go:157`). Bytes that
     permanent storage refuses quarantine the whole delivery, as `importDeliveryTo` does. Step 1
     is `tenancy.MailboxImport` (`tenancy/imports.go:83`): the owner's org from `subject_orgs`,
     then the cache's last known status (not cached or no org waits; `deleted` waits for the
     purge; every other status imports, `enabled` or not). Step 2 (`writeObligation`, `:182`):
     under the shared authority fences an owner or address that no longer matches is retired, as
     `frozenAuthority` reads it; any other refusal is retired only if `bindingsRetired`'s
     directory-locked read proves it, as `quarantineRetired` does. Otherwise the mailbox's copy
     is written under its quota lock (`writeMailbox`, `:214`), after re-reading the org there:
     `deleted` writes nothing (the purge fence). The lease is renewed there too, so a worker
     whose lease ran out while it waited for the fences or the lock writes nothing and cannot
     overtake the mailbox's new holder. The write is `ingress.Store.ImportMailbox`
     (`ingress/obligations.go:92`), one receipt for all the mailbox's bindings, with no claim.
   - **Completion** (`complete`, `app/receiving_imports.go:297`; `ingress.Store.Complete`,
     `ingress/obligations.go:133`). Reading: the done-state (`mailbox.Store.HasReceipt`, or a
     `retired_bindings` row) is read after the caller's own write commits and before the
     `kypost_ingress` writer transaction, which re-reads the bindings. Of two workers, the later
     committer sees both done, and the transaction ends the delivery once. No `FOR UPDATE`: the
     ingress writer lock serializes it. Archived as `imported` when none is retired; otherwise
     the imported bindings are deleted and the delivery is quarantined with only the retired
     ones (lease cleared, so a discard says `discarded`). A delivery under a live single-mode
     claim is left alone.
   - **Backstop sweep** (`sweepCompletions`, `app/receiving_imports.go:315`). Every minute on
     the elected `daemon` role (`app/receiving.go:346`), which in multi mode runs only the sweep.
     Reading: not on `api-sweepers` as R8 says, because only the daemon process opens the
     receiving runtime. It walks `PendingGateways` (`ingress/obligations.go:29`), every gateway
     with pending mail, never the pins.
   - **Single mode** never calls any of it: `startLeasedImport` returns at once, and
     `startReceivingImport`'s two-gateway loop is unchanged byte for byte below the new
     multi-mode branch.
   - **Tests** (`app/receiving_imports_test.go`, all on both backends): `TestLeasedImportOutlivesItsGatewayPin`
     (revoked and expired pins, the other pin, a second rotation, re-enrolment, revoked key and
     retired ID refused, over TLS; the store rules are PR 2's `TestGatewayPins`),
     `TestLeasedImportTwoReplicasKeepOrder`, `TestLeasedImportDeletedOrgWaitsForItsPurge`,
     `TestLeasedImportSuspendedOrgImports`, `TestLeasedImportUncachedOrgWaits`,
     `TestLeasedImportPurgeFence` (the re-read alone, a worker waiting on the purge's lock, a
     lease lost while waiting, and write first), `TestLeasedImportCrashBeforeCompletion`,
     `TestLeasedImportConcurrentCompletion`, `TestLeasedImportRetiredBinding` (active plus
     retired, and a purge leaving only retired), `TestLeasedImportLeases` (killed holder, and the
     100 bound with its skipped pass), and `TestSingleModeKeepsTheDaemonImporter`. Also
     `ingress` `TestObligations`, `TestCompleteLeavesAClaimedDelivery`; `tenancy`
     `TestImportLease`, `TestRetiredBindingsAndOrgImport`; and
     `TestSingleTenantUpgradeIsByteForByte` unchanged. Each of these mutations fails its test:
     no re-read under the quota lock, a retired binding counted as imported, the skip ignored,
     `deleted` importing, a lease taken while live, an uncached org importing, and no lease
     check under the quota lock.
   - **K PR 6 stand-in** (replaced in PR 5 by the real purge, below). The deleted-org and retired-purge tests used `purgeStandIn`, written
     against K "Purge" steps 2–3: delete the org's bindings and `retired_bindings` rows, delete a
     delivery left with none, run Completion, then remove storage under each quota lock. Switch
     `TestLeasedImportDeletedOrgWaitsForItsPurge`, `TestLeasedImportPurgeFence` and
     `TestLeasedImportRetiredBinding` to the real purge once K PR 6 merges, and have the purge
     call `ingress.Store.Complete` for its step 2.
   - **Not in PR 4.** The 60 s time bound has no test of its own (the 100 bound does).
     `HeldBytes` still counts a delivery against a mailbox that already imported it until the
     delivery completes, so a mailbox waiting on another org's purge or poll can refuse new mail
     (452) a little early; it is bounded by that wait. Cloudflare's importers still take the
     whole-delivery claim, which `Complete` leaves alone (R1 keeps Cloudflare out of multi mode).
5. **Hosted profile.** Compose for MX hosts and per-replica Rspamd, the relay domain as a
   `destination`, `{source_ip}` on `bind`, exit 12 for K's org check, and exit 13 with the
   per-source budget. Tests:
   `TestCrossOrgMatrix`'s SMTP `RCPT` row against this profile (K), a suspended org's address
   answering `451 4.2.1` and a deleted one answering `550 5.1.1`, probing beyond the budget
   answering `450` for live and unknown relay addresses alike, primary domains never budgeted, and
   M's hosted verification list.
   *PR 5 as built:* paths under `backend/internal/` at the PR's head. Startup still refuses
   `multi` (K PR 7), so the tests build the multi runtime directly, as K's do
   (`multiRuntime`, `app/receiving_org_test.go:26`, sets `tenancy.db` and `state.db`).
   - **Org check** (`orgReceives`, `app/receiving_probe.go:38`, called after the assignment at
     `app/receiving.go:630`). It applies K PR 5's `tenancy.Org.Receiving` to the owner's
     `subject_orgs` org. Suspended, disabled or not cached is exit 12. `deletion_scheduled` and
     `deleted` are `ingress.ErrRoute`, so they answer byte for byte as an unknown address does.
     Readings:
     - A subject with no `subject_orgs` row is exit 12: a binding gap that repair fixes, so
       temporary.
     - The staff org always receives (K8), as `tenancy.Gate` treats it.
   - **The relay domain** is the install's masked-address domains (`state.MaskedDomainsSetting`,
     `state/backup.go:25`, moved there from `api` unchanged). Reading: no new setting. M D2 puts
     hosted masked addresses on the relay domain and keeps it apart from the primaries, so the
     masked list is the relay list. A primary domain that staff also listed as masked would be
     budgeted; D2 rules that out. The relay domain is already a `destination`, because every
     configured domain is one (`destinations`, `app/receiving_config.go`).
   - **`{source_ip}` on `bind`.** Only the hosted profile renders it (`bindSource`,
     `app/receiving_config.go:162`). `checkReceivingArgs` (`app/receiving.go:258`) accepts the
     fifth argument only when `KYPOST_TENANCY=multi`, and only as one literal IP with no zone.
     Maddy 0.9.5 expands `{source_ip}` at `RCPT` with the same function as at DATA
     (`internal/check/command/command.go:171-178` and `:356-364` in Maddy's v0.9.5 source; read,
     not run). A non-TCP connection gives an empty string, which is refused as exit 1.
     - A single-tenant five-argument `bind` stays a usage error.
     - A four-argument `bind` in multi mode still works for primary domains, so the callers from
       PRs 2–4 are unchanged.
     - A relay-domain `bind` without the IP is exit 1 (`errProbeSource`). It is never let through
       unbudgeted.
   - **Budget** (`probeBudget`, `app/receiving_probe.go:69`; defaults at `:24`).
     - It runs before the sender checks, so past the budget nothing else is consulted.
     - Only exit 3 refusals count (M's "N unknowns"). Paused, expired and deleted masked
       addresses and deleted orgs count, because they answer as unknown. Exits 6–12 do not.
     - The windows are R's `security_windows` in `state.db` (`TakeAttempt`), keyed
       `receiving-probe:<prefix>`. The source is unmapped from IPv4-in-IPv6 and masked to /32 or
       /64.
     - The 50th refusal is still answered `550`, and writes `receiving-probe-block:<prefix>` for
       10 minutes. While that row lives, every relay-domain `RCPT` from the prefix is exit 13.
     - Reading: a refusal after the block ends, but inside the same 10-minute count window,
       blocks again at once.
     - A window that cannot be read or written is exit 1. Primary domains never read or write
       one.
   - **Compose.** One profile in two files. `scripts/check-replicas-compose.py` validates both
     with `docker compose config`. Neither can start until K PR 7.
     - `docker-compose.hosted.yml` goes after the replica chain. It adds `receiving-a` and
       `receiving-b`: `receiving serve --tls` on the shared volumes and database, read-only,
       unprivileged and with no capabilities, published only on `KYPOST_RECEIVING_PUBLISH_IP`.
     - Each server has its own Rspamd (`rspamd-a`, `rspamd-b`) in its network namespace. The
       image is pinned as in `docker-compose.rspamd.yml`, and the check fails if the two drift.
     - The file also sets the replicas to multi mode with native receiving, so every replica's
       daemon imports.
     - `docker-compose.mx.yml` is the MX host alone: Maddy through `start-receiving.sh`,
       read-only binds, Maddy's state in memory, and no volume, database or scanner.
     - Reading: the receiving servers are their own containers, not sidecars of the replicas,
       because `deploy.replicas` cannot give each copy its own Rspamd. Two are defined; add more
       by copying a pair.
     - `start-receiving.sh`'s MX branch re-renders every 5 minutes, restarts Maddy only when the
       profile changed, and exits when Maddy dies. `scripts/check-receiver-launch.py` runs it
       with a stand-in engine.
     - MH7's parts are left for MH7 to add to these same files: separate volumes per host, a
       read-only `SECRET_DIR`, `sslmode=verify-full`, keepalives and pooling.
   - **Purge, switched from the stand-in.** `TestLeasedImportDeletedOrgWaitsForItsPurge`,
     `TestLeasedImportPurgeFence` and `TestLeasedImportRetiredBinding` now run K PR 6's own steps
     2 and 3, then the Completion sweep. The steps are exported for them (`api.PurgeReceiving`,
     `api/tenancy_purge.go:234`; `api.PurgeStorage`, `:272`).
     - Reading: the purge does not call `ingress.Store.Complete`, as PR 4's note proposed.
       Completion's done-state needs the receiving runtime's mailbox reads, which only the daemon
       opens. The backstop sweep archives a delivery the purge left with only finished bindings,
       within a minute.
     - A delivery bound only to the purged mailboxes is now archived `discarded` (the real step
       2). The stand-in deleted it.
   - **Tests** (`app/receiving_hosted_test.go`):
     - `TestHostedBindOrgStatus`: active, suspended, disabled, uncached, `deletion_scheduled`,
       `deleted`, staff.
     - `TestHostedBindProbeBudget`: 50 refusals, then exit 13 for live, plus-tagged, masked and
       unknown addresses alike. A paused masked address is counted like an unknown one, and
       sender refusals are not counted. It also covers an IPv4 /32, an IPv4-mapped address, an
       IPv6 /64 and a missing peer IP.
     - `TestHostedBindProbeBlockEnds` and `TestHostedBindPrimaryDomainsNeverBudgeted`.
     - `TestCrossOrgMatrixSMTPRcpt`, K's SMTP `RCPT` row. A bind and accept with the peer IP, over
       TLS, deliver to B's address into B and never A. Probing over the same path then ends in
       exit 13.
     - `TestHostedBindArguments`.
     - Changed tests:
       - `TestReceivingTLSExitCodesAndHostedProfile` now allows `{source_ip}` as the hosted
         profile's one other difference.
       - Exit 12 at `bind` refuses uncached orgs, so `TestLeasedImportUncachedOrgWaits` now
         empties the cache after acceptance, as a restore does (`tenancy.ForgetRestored`).
       - `TestReceivingOrgStorageQuotas` expects exit 12 for an unbound subject.
     - Unchanged and passing: `TestSingleTenantUpgradeIsByteForByte` and the single-tenant
       `receiving.conf` golden.
     - Each of these mutations fails a test: no org check, no budget, the budget after the other
       checks, IPv6 per /128, the staff org gated, and every refusal counted.
   - **M's hosted verification list.**
     - Built here: unknown, paused and deleted masked addresses are refused alike on the relay
       domain, and counted alike.
     - Not here, because they are M changes 1–3 and 5 in K PR 7: the cross-org matrix for masked
       routes, the per-org masked quota test, and the native-mail harness run with KyIdentity in
       multi mode (which needs a startable multi mode).
     - Tombstone survival across org deletion is K PR 6's (`TestOrgPurgeRunsAgainAfterARestore`).
   - **Not run in PR 5:** a real Maddy run of the hosted profile, Maddy restarting under load
     in the MX image, and a container start of either file.
6. **Capacity measurement**, recorded in this spec.

Past one host (R's follow-up, PostgreSQL as its own service) is outside this plan, as in K.

## Residual risks

- **MX hosts see mail in transit.** Inherent to running MX. Hosts are ours, hold nothing at rest,
  and require TLS from senders.
- **SPF input from the MX host.** A compromised MX host can lie about the peer IP. DKIM alignment
  limits the damage.
- **Duplicates.** A lost answer after commit gives `451`, and the retry is stored twice.
- **Probe budget slows shared IPs.** A provider IP that trips the relay-domain budget is
  temp-failed for every customer until the block ends.
- **Staged orphans.** Each MX crash mid-transaction leaves a staged row until fenced cleanup runs.
- **One host for replicas** until R's follow-up. MX hosts are redundant, but the replicas and
  PostgreSQL are not.
- **Shared reputation and abuse.** As M and K.
- **Unmeasured capacity.** Every figure here is a default, not a measurement.

## Open questions, with recommendations

1. **Should self-hosted R replicas get the tier?** *Recommend* no in v1. The tier is gated on
   multi mode, which keeps single-tenant unchanged. Revisit when R's follow-up lands.
2. **Gateway enrolment: CLI only, or also a staff admin route?** *Recommend* CLI only in v1, on a
   replica, with an audit line. It is rare operator work, and every new admin route adds attack
   surface.
3. **Pin lifetime.** *Recommend* 90 days, with a warning on the staff console at 14 days left.
4. **An index on `bindings(mailbox)`.** None is proposed, so `kypost_ingress` keeps one schema
   in both modes (K3). *Recommend* adding one only if the capacity measurement shows the
   per-mailbox scan is slow. It would need the same exact-schema and capsule-copy review that K's
   "PR 1 as built" applied to `tenancy.db`.
5. **The per-source refused-recipient budget.** *Recommend* counting relay-domain refusals only, per
   source IP (IPv4 /32, IPv6 /64): 50 refusals in 10 minutes, then `450 4.7.1` for 10 minutes. The
   ceiling is a shared provider IP being slowed for everyone (Masked relay domain). Tune it after
   the capacity measurement has counted real provider IPs.
6. **Hosted ingress limits.** *Recommend* setting them from the capacity measurement before
   launch (they can only ever be raised). Track import lag against an alert at half of each limit.
7. **Message-ID de-duplication on import.** Duplicates are possible after a lost answer.
   *Recommend* leaving import as it is (at-least-once) and measuring how often it happens before
   adding de-duplication, because de-duplicating by a sender-written header can drop real mail.
8. **Cloudflare's own figures.** No inbound rate limit is documented. *Recommend* no action:
   hosted mode does not use Cloudflare (R1).
