# KyPost hosted receiving tier, design

Date: 2026-11-21. Status: proposed design, nothing built. Docs only.

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
| R7 | In multi mode, sender blocks and evidence live in PostgreSQL tables in `kypost_ingress`, with the same semantics. Single-tenant keeps the JSON files. | A whole-file rewrite per message does not scale. Hosts past one need shared state. Multi installs start empty (K2), so nothing is migrated. |
| R8 | Import is leased **per mailbox** in PostgreSQL, so every replica imports. A delivery bound to several mailboxes needs leases on all of them, taken in ID order. | Spreads import across replicas while keeping each mailbox's arrival order. Mirrors `quotaFence`'s lock order (`app/receiving.go:54-66`). |
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
         PostgreSQL: kypost_ingress (deliveries, bindings, archived,
         sender_blocks, sender_evidence, import_leases, receiving_gateways)
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
| Enrol | On a replica, `kypost-server receiving gateway add <id> <spki-sha256>` (operator CLI, like `receiving blocks`) writes a `receiving_gateways(id, spki_sha256, created_at, expires_at, revoked_at)` row. `expires_at` defaults to 90 days. The command writes an audit line with the ID and the hash prefix. A staff admin route can come later (open question 2). |
| Rotate | Generate a new key on the host, enrol it under the **same** ID (at most two live pins per ID), switch the helper, then revoke the old pin. The overlap keeps mail flowing. A pin past `expires_at` is refused like a revoked one. |
| Revoke | `receiving gateway revoke <id> [<spki>]` sets `revoked_at`. Every replica re-reads the table for each new connection (or caches it for at most 5 seconds), and connections last one command (at most 35 seconds), so a revocation takes effect within about 40 seconds. Revoking an ID does not touch its deliveries. Its staged rows wait for the fenced cleanup, and its committed mail still imports. |
| Server keys | Each replica's listener key is created exclusively in `SECRET_DIR` (as R's keys are) and printed by `receiving gateway server-pin`. MX helpers pin the current and the next one. |

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

### Sender blocks and evidence in PostgreSQL (R7)

- **Tables** (multi-only, in `kypost_ingress`, class P in K's inventory):
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

- **Table.** `import_leases(mailbox PK, holder, lease_until)` is multi-only, in `kypost_ingress`. A
  multi-only index on `bindings(mailbox)` lists one mailbox's pending deliveries. K3 forbids
  columns added in one mode only. An index adds no column, but it does change the schema dump, so
  it is open question 4.
- **Loop.** Every replica runs import workers, not only the `daemon` replica. A worker takes a
  lease of 60 seconds on mailboxes with pending mail (`UPDATE … WHERE lease_until < now() …
  SKIP LOCKED`), renews it while working, and imports each mailbox's deliveries in arrival order
  through today's `importDelivery`. The per-delivery claim lease (`ingress/store.go:830-884`) and
  the quota lock still fence everything, so a lease bug can cost throughput but not
  correctness.
- **Multi-mailbox deliveries.** The worker needs leases on every bound mailbox, taken in sorted
  ID order (`quotaFence`'s order, `app/receiving.go:54-66`). If one is busy it releases them all
  and retries later, so it never waits holding a lease.
- **Gateways.** The import list is every gateway ID that has pending deliveries
  (`SELECT DISTINCT gateway FROM deliveries WHERE state='pending'`), not the two fixed IDs of
  `app/receiving.go:230`, and never the live rows of `receiving_gateways`. A revoked or expired
  pin stops new commands only. Mail its host already committed has had its `250` and still
  imports. `receiving_gateways` rows are never deleted, so a revoked ID is never enrolled again.
- **Org status.** K's rule holds. A delivery whose org is not `active` stays pending
  (K "Background workers").
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
  `{source_ip}`, and the replica counts refused recipients per source (/24 for IPv4, /64 for
  IPv6) in R's PostgreSQL windows. Past N in a window, every further `RCPT` from that source,
  live or not, gets a new exit code 12, `450 4.7.1`, so the answer reveals nothing. Codes 0–11 do
  not change, and single-tenant configurations never contain code 12. N and the window are open
  question 5.
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
  `receiving_gateways`, `import_leases`, `sender_blocks` and `sender_evidence` tables, the
  `bindings(mailbox)` index and exit code 12 are each refused or never created unless
  `KYPOST_TENANCY=multi` is recorded (K1).
- **Proof.** K PR 1's single-tenant upgrade test, extended: a single-container Maddy fixture and
  an R replica fixture started under the new binary keep every `kypost_ingress` row and schema
  dump, `sender-blocks.json`, `sender-evidence.json` and the generated `receiving.conf`
  byte-for-byte unchanged. `TestEveryStoreIsClassified` gains the four new tables.
- Self-hosted R replicas with their own MX hosts would be possible later, but this spec gates the
  tier on multi mode (open question 1).

## Threat model

| Attacker | Goal | Defence |
|---|---|---|
| Internet sender | Deliver into the wrong mailbox or org | Unchanged routing by address on the replica (`bind`), with K's org check. The MX host decides nothing. |
| Internet sender | Probe for live addresses (primary or masked) | Identical `550 5.1.1` for unknown, paused, expired and deleted addresses (`app/receiving.go:448-457`). The per-source budget answers `450` to every `RCPT` after N refusals, whatever the address. M's address entropy. |
| Internet sender | Exhaust receiving | Maddy limits per MX host. The ingress record and byte limits refuse with a temporary failure, never by evicting mail (`ingress/AGENTS.md`). Per-mailbox quota (exit 9). |
| Host on the private network without a pinned key | Inject or read mail | `RequireAnyClientCert` plus the pin check before any frame is read. The listener binds only to the private address. |
| Compromised MX host | Read mail in transit | It sees the mail it receives. That is inherent to running an MX and the reason it holds nothing else. It cannot read stored mail: it has no mailbox access, and the protocol has only `bind`, `accept` and `config`. |
| Compromised MX host | Forge or replay deliveries, or touch another host's | It can submit mail as a sender could, under its own gateway ID only (R4). It cannot acknowledge, release or import. Revoking its pin cuts it off within about 40 seconds. |
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
2. **TLS transport.** `receiving serve --tls`, protocol 3 (`budget_ms`), `receiving_gateways`, the
   `receiving gateway keygen|add|revoke|server-pin` CLI, `KYPOST_RECEIVING_REMOTE` with no local
   fallback, the pinned gateway ID, and the `config` fetch. All refused outside multi mode. Tests:
   - wrong client key, revoked pin, expired pin and a reserved gateway ID, each refused before any
     frame is read;
   - wrong server key, refused by the helper;
   - replay of an archived `accept`, which succeeds and writes nothing;
   - a new recipient on an archived ID, refused;
   - an oversize body, and a `length` that does not match;
   - a hung replica, which gives exit 1 within the budget;
   - `bind` and `accept` landing on different replicas;
   - every exit code 0–11 mapping to the same SMTP answer as on the socket;
   - single-tenant: `--tls` refused, protocol 2 unchanged.
3. **Sender blocks and evidence in PostgreSQL** (R7), behind one interface. Tests: the
   `blocks.json` fixture, today's sender-block tests and `TestEvidenceWorstCaseSize`, all run
   against both implementations. Multi mode never touches the files.
4. **Per-mailbox import leases** (R8) and the pending-gateway import list. Tests:
   - commit through a gateway, revoke its pin (and separately, let it expire), and the mail still
     imports;
   - re-enrolling a revoked gateway ID is refused;
   - two replicas importing one mailbox keep arrival order and import nothing twice;
   - a multi-mailbox delivery with one lease busy;
   - a lease holder killed mid-import;
   - a non-`active` org's delivery stays pending.
5. **Hosted profile.** Compose for MX hosts and per-replica Rspamd, the relay domain as a
   `destination`, `{source_ip}` on `bind` and exit code 12 with the per-source budget. Tests:
   `TestCrossOrgMatrix`'s SMTP `RCPT` row against this profile (K), probing beyond the budget
   answering `450` for live and unknown addresses alike, and M's hosted verification list.
6. **Capacity measurement**, recorded in this spec.

Past one host (R's follow-up, PostgreSQL as its own service) is outside this plan, as in K.

## Residual risks

- **MX hosts see mail in transit.** Inherent to running MX. Hosts are ours, hold nothing at rest,
  and require TLS from senders.
- **SPF input from the MX host.** A compromised MX host can lie about the peer IP. DKIM alignment
  limits the damage.
- **Duplicates.** A lost answer after commit gives `451`, and the retry is stored twice.
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
4. **The multi-only `bindings(mailbox)` index.** K3 forbids mode-only columns; an index is not a
   column, but it changes a multi schema dump. *Recommend* allowing it as a named K3 exception:
   no query branches on it, and single-tenant never has it.
5. **The per-source refused-recipient budget.** *Recommend* 20 refusals in 10 minutes per /24 or
   /64, then `450 4.7.1` for 10 minutes. Tune after measurement.
6. **Hosted ingress limits.** *Recommend* setting them from the capacity measurement before
   launch (they can only ever be raised). Track import lag against an alert at half of each limit.
7. **Message-ID de-duplication on import.** Duplicates are possible after a lost answer.
   *Recommend* leaving import as it is (at-least-once) and measuring how often it happens before
   adding de-duplication, because de-duplicating by a sender-written header can drop real mail.
8. **Cloudflare's own figures.** No inbound rate limit is documented. *Recommend* no action:
   hosted mode does not use Cloudflare (R1).
