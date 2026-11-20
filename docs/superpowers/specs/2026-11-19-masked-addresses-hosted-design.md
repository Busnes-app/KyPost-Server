# Masked and plus addresses: self-hosted, hosted SaaS and isolated platforms, design

Date: 2026-11-19. Status: proposed design, nothing built. Docs only.

Owner decision (fixed, not reopened here):

- Hosted masked addresses come from **one shared platform relay domain**, unique
  across all customers. Per-customer custom domains come later.
- Self-hosted keeps its own domain.

Line references are to KyPost `origin/main` at `28d91bc2`, and to KyIdentity
`origin/master` at `a86964c` for the tenancy spec
(`docs/superpowers/specs/2026-11-19-tenancy-design.md`, cited below as **T**).
"Unverified" marks a claim this note did not check in code or by running it.

## Summary

| | Self-hosted | Isolated platform (business) | Hosted SaaS (individuals, households) |
|---|---|---|---|
| KyIdentity mode | single-tenant | single-tenant, own install (T decision 7) | multi-tenant, tokens carry `org_id` (T D4) |
| KyPost install | one per operator | one per business | one shared install, data separated by org (T decision 2) |
| Masked domain | the operator's own, chosen by the administrator | the business's own, chosen by its administrator | one platform relay domain, chosen by staff; per-customer domains later |
| Who may enable it | administrator (`PUT /api/admin/masked-domains`) | the business's administrator | staff only (T decision 6: no customer administrators) |
| Uniqueness scope | the install's ledger | the install's ledger | the shared install's ledger, which covers every customer |
| Address format | today's `adjective.animal.NNNN` | today's | longer format on the relay domain (D3) |
| Plus addressing | today's, on by default | today's | today's, on primaries and on masked addresses |
| Changes to KyPost | none | none | needs KyPost's own tenancy work first (D6) |

## How it works today (self-hosted)

This is the baseline. **It stays unchanged in every mode.**

- **Domains.** The administrator lists the masked domains in root `state.db` meta
  `masked_address_domains` (`backend/internal/api/masked_addresses.go:29`). Each must
  be a configured native mail domain, proved by the `_kypost-mail` TXT record bound to
  the KyIdentity issuer (`backend/internal/sso/native_domain.go:56-62,349-353`). The
  routes are `GET|PUT /api/admin/masked-domains`, with admin step-up
  (`backend/internal/api/server.go:735-736`). None listed means the feature is off.
- **Creation.** `POST /api/addresses/masked` (`masked_addresses.go:327`):
  - The local part is `adjective.animal.NNNN`, drawn with `crypto/rand`
    (`masked_addresses.go:51-82`).
  - The word lists hold 63 adjectives and 66 animals, with 9000 numbers, which gives
    37,422,000 local parts per domain. The code comment says "about 40 million".
  - Up to 5 draws are tried against the ledger (`NativeAddress`, `masked_addresses.go:376-383`).
    The address is then written as a ledger alias on the user's primary mailbox
    (`AddNativeAlias`, `backend/internal/sso/native_address.go:196`), which refuses an
    address that already exists (`:201-202`) and any local part holding `+` (`:197`).
- **Limits.** 100 addresses not deleted per user, and 20 created in any 24 hours,
  deleted ones included (`masked_addresses.go:31-37,358-374`). Notes are at most
  100 characters. Disposable lifetimes are 24h, 7d or 30d (`:40`).
- **State.** The user's record lives in their `config.yaml`
  (`config.UserSettings.MaskedAddresses`, `backend/internal/config/config.go:210-226`). Mail
  flows only while that record is live and the ledger address is active
  (`config.go:228-245`).
  - Pause and delete reserve the ledger address (`maskedClose`, `masked_addresses.go:185`).
  - Resume hands it back to the same mailbox (`maskedOpen`, `:198`).
  - A minute sweep closes expired and orphaned records (`sweepMaskedAddresses`, `:479`,
    called from `native_provisioning_worker.go:123`).
- **Never reused.** The ledger never deletes a record (`docs/NATIVE_PROVISIONING.md`,
  "Plus addressing and masked addresses"). An administrator's release or reassign of a
  user's masked address gets `409` (`refuseMasked`,
  `backend/internal/api/native_address_handlers.go:84-97`).
- **Receiving.**
  - With Maddy, `bind` resolves the plus base (`NativeAddressFor`,
    `native_address.go:89-97`) and refuses through `RefusesAddress` exactly as for an
    unknown address (`backend/internal/app/receiving.go:482-491`).
  - With Cloudflare, the published routing table omits non-live masked addresses
    (`backend/internal/app/receiving_cfcontinuous.go:411`). The Worker matches a full
    route first, then the plus base (`receiving-worker/continuous.mjs:294-301`).
  - Both are tested in `TestNativeReceivingMaskedStates` and
    `TestCloudflareRoutesPlusAndMasked`
    (`backend/internal/app/receiving_plus_masked_test.go:69,113`).
  - Browser evidence is in `docs/ui-native-mail-harness-2026-11-19/` (PR #675):
    paused and deleted addresses get the same `550 5.1.1` as an unknown one.
- **Sending.**
  - A plus-tagged or masked From is allowed only for the sending mailbox, while live
    (`nativeFrom`, `backend/internal/api/native_outbound.go:155-173`).
  - From and the envelope sender are the masked address, and no header names the
    primary (`TestMaskedSendLeaksNoPrimaryAddress`, `masked_addresses_test.go:197`).
  - Direct delivery DKIM-signs `d=` the masked domain with no `i=`
    (`TestDKIMSignatureCarriesNoIdentity`, `backend/internal/mailmsg/dkim_test.go:323`).
  - A relay profile is never DKIM-signed by KyPost; the provider signs
    (`docs/DOMAIN_RELAY.md`, first paragraph).
  - Masked mail counts toward the owner's sending quota (`sendQuotaFor`,
    `backend/internal/api/send_quota.go:25`).
- **Plus addressing.** On by default, with a per-user refusal (`config.go:235-237`).
  The web client replies from the address the delivery receipt names.

## Decisions this note proposes

| # | Decision | Why |
|---|---|---|
| D1 | Self-hosted and isolated platforms keep today's behaviour, byte for byte. An isolated platform is a self-hosted install that staff set up for a business (T decision 7). | Owner decision. Both are single-tenant, so nothing in the code distinguishes them. |
| D2 | Hosted masked addresses live on one **platform relay domain**, separate from the domain(s) that hold hosted primaries (for example `relay.<platform>` beside `<platform-mail>`). Staff are its only administrators. | A separate domain keeps masked and primary namespaces from colliding, gives masked mail its own reputation, and keeps primaries working if the relay domain is blocklisted. Owner decision fixes "one shared domain"; keeping it separate from primaries is this note's choice. |
| D3 | On the relay domain only, the local part is `adjective.animal.` plus 6 random characters from the lowercase RFC 4648 base32 alphabet (`a`–`z`, `2`–`7`), for example `calm.raven.k7q2xm`. Lowercase, because KyPost lowercases addresses before lookup (`masked_addresses.go:219`, `:429`). 63 × 66 × 32⁶ ≈ 4.5 × 10¹² local parts. Self-hosted keeps four digits. Six decimal digits were considered and rejected: 63 × 66 × 900,000 ≈ 3.7 × 10⁹, only 100 times today's space. | 37 M per domain is fine for one household. Shared by every customer, it is not. With 2 M live addresses, a random guess hits a live one about 1 time in 19 today (2 × 10⁶ / 3.74 × 10⁷). With base32 that becomes about 1 in 2.2 million. The 5-draw collision retry (`masked_addresses.go:376`) also stops being a concern (see below). |
| D4 | Uniqueness across customers comes from the **one ledger of the one shared KyPost install**: today's `AddNativeAlias` conflict check plus the never-delete rule. No new allocator, as long as hosted is one install. | It already holds: one install, one ledger, global address keys. A second hosted install (sharding) would need a shared allocator (open question 1). |
| D5 | A deleted address, and every address of a deleted org, stays as a ledger **tombstone** forever. When an org is deleted, the tombstone keeps the address (or a keyed hash of it, open question 4) and drops the mailbox, owner and note. | "Never reuse" must survive org deletion. Today's ledger keeps the full record, which would retain the deleted customer's data. |
| D6 | Hosted masked addresses ship **only on top of KyPost's own multi-tenant work** (T "Downstream contract"). This note adds the masked-specific deltas; it does not design KyPost tenancy. | A shared install without org isolation is not safe for any feature. Masked addresses add no new isolation primitive. |
| D7 | Hosted limits come from KyIdentity's per-app quotas (`org_apps.quotas_json`, T "Entitlements", read via `GET /api/suite/orgs`): `maskedPerUser`, `maskedPerDay`, `maskedPerOrg`. Today's 100/20 stay the defaults and the self-hosted values. | T D6 assigns quota enforcement to apps. Today's limits are per user, so a household of 6 could make 120 a day; a per-org cap bounds that. |
| D8 | Masked state moves from `config.yaml` into the ledger (or a per-account table beside it) in hosted mode, carrying `org_id`. | `maskedHeld` (`masked_addresses.go:218-233`) and the minute sweep (`:479-500`) read every user's `config.yaml`. The sweep is O(users) every minute, which does not scale to a shared install (unverified: no measurement). The ledger is also where the org boundary must be enforced. |

## Hosted SaaS

### Address generation and uniqueness across tenants

- **Generation.** Same code path, with a format per domain class (D3). The domain class
  comes from configuration (the relay domain is marked platform-owned), never from the
  request.
- **Uniqueness.**
  - One shared install and one ledger: an address exists at most once
    (`native_address.go:201`), and is never deleted (D5). This covers every customer
    because the ledger is not partitioned by org. Org scoping applies to who may *see or
    change* a record, not to the address key space.
  - The 5-draw retry stays. With D3's space, the chance that all 5 draws collide at
    10⁸ live addresses is about (10⁸ / 4.5 × 10¹²)⁵ ≈ 5 × 10⁻²⁴. With six digits it
    would be about (10⁸ / 3.7 × 10⁹)⁵ ≈ 10⁻⁸, which is the main reason to prefer base32.
  - A refusal (`ErrNativeAddressConflict`) is retryable by the user and leaks nothing.
- **Plus addressing on the relay domain.** `calm.raven.k7q2xm+shop@relay` resolves to the
  masked base (`NativeAddressFor`, `native_address.go:89`). It is delivered only while the
  masked address is live and its owner accepts plus addresses. Nothing to change.
- **Primaries.** Hosted primaries come from KyIdentity, where username = email and is
  globally unique (T D5). A primary on the hosted mail domain can never equal a masked
  address, because they are on different domains (D2).

### Enumeration and abuse

| Vector | Today | Hosted change |
|---|---|---|
| RCPT probing of the relay domain's MX | Live answers `250`, everything else answers the same `550 5.1.1`. Maddy limits per IP (`ip concurrency`, `ip rate … 1m`, `backend/internal/app/receiving_config.go:186-188`). | D3's larger space makes a hit rare. Add a per-source budget for refused recipients (tarpit or temporary refusal after N unknowns in a window). Unverified: Cloudflare Email Routing gives no RCPT-time refusal. The Worker answers after acceptance with a bounce (`docs/guides/NATIVE_MAIL.md` §8), so probing there costs the prober a bounce per guess, but still reveals liveness. |
| Mass creation (address farming for sign-up abuse) | 20 per user per 24 h, 100 live per user | Per-org cap (D7), plus a platform-wide circuit breaker on creations per minute. Staff alert when it trips. |
| Spam *from* masked addresses through the shared relay | The owner's sending quota (`send_quota.go:25`) | The relay domain's reputation is shared by every customer, so one abusive customer harms all. Per-org send quotas from `quotas_json`, and staff can suspend masked sending for an org (org suspension in T already stops everything). The provider's own complaint feedback goes to staff. Unverified: KyPost has no provider-feedback (bounce or complaint) ingestion today. |
| Correlating customers | A masked address names no user, and replies carry no primary header (`TestMaskedSendLeaksNoPrimaryAddress`) | Unchanged. The Message-ID domain is the relay domain (`docs/NATIVE_PROVISIONING.md`), so one sender's masked addresses are linkable only by timing and content, not by headers. |

### What the relay domain needs

- **DNS.**
  - MX to the hosted receiving profile.
  - The `_kypost-mail` TXT proof bound to the hosted issuer (`native_domain.go:56-62`). One
    issuer per hosted install, so nothing changes.
  - SPF, DKIM and DMARC as below.
  - The relay domain is a platform asset: only staff edit its DNS, never a customer.
- **Receiving.** Either profile works unchanged, because routing is by address, not
  by org.
  - With Cloudflare, the published routing table lists every live masked address of
    every customer (`docs/guides/NATIVE_MAIL.md` §1 privacy row). That is one
    provider-visible list of all hosted masked addresses. Accepted or not is open
    question 5.
  - With Maddy, one generated `destination` covers the domain
    (`backend/internal/app/receiving_config.go`); recipients are checked per RCPT.
- **SPF, DKIM and DMARC for replies From masked addresses.**
  - **Relay mode** (the provider signs): the provider must be authorized for the relay
    domain and sign with `d=` the relay domain (aligned DKIM). Use a custom MAIL FROM
    subdomain (for example `bounce.relay.<platform>`) with its SPF record, so SPF aligns
    too. KyPost itself signs nothing (`docs/DOMAIN_RELAY.md`).
  - **Direct mode**: KyPost signs `d=` the relay domain with that domain's key
    (`backend/internal/mailmsg/domain_relay.go:98`), so a key compromise covers every
    customer's masked mail. Rotate per `docs/DIRECT_DELIVERY.md`.
  - **DMARC** on the relay domain: start at `p=quarantine`, `adkim=s`, `aspf=r`, move to
    `p=reject` once reports are clean. Aggregate reports go to staff.
  - Recommendation: relay mode with a provider that supports per-domain custom MAIL FROM.
    Direct mode concentrates every customer's reputation on our own IPs.
- **Bounces.**
  - The envelope sender is the masked address (`docs/NATIVE_PROVISIONING.md`), so remote
    DSNs return to the relay domain's MX.
  - While the address is live, the DSN is delivered to the owner's mailbox (null sender,
    plus addressing irrelevant).
  - While it is paused, expired or deleted, the DSN is refused like any mail to it.
    The remote MTA then drops it as a double bounce, so a user who pauses an address
    stops seeing its bounces. That is intended.
  - With a custom MAIL FROM at the provider (above), bounces go to the provider instead,
    and only reach KyPost if a feedback path is built (unverified that one exists; see
    the abuse table).
- **Deletion and reuse.**
  - A masked address is never reissued, in any mode (existing rule).
  - Hosted adds: org deletion (T "deletion_scheduled" → `deleted`) keeps a tombstone for
    every ledger address the org ever held (D5).
  - A suspended org's masked addresses temporarily refuse mail (open question 2). They are
    not released.
  - Removing the relay domain from service is not supported while any tombstone exists on
    it. In practice the relay domain is permanent; plan its name accordingly.

### Per-customer custom domains (later)

Out of scope for v1, recorded so v1 does not block it:

- A custom domain belongs to exactly one org. Its proof (`_kypost-mail` TXT) is bound to
  the issuer *and* the org, so another org cannot claim it by publishing the same record.
- Masked addresses on it are unique within that domain, and so within that org.
- The never-reuse rule binds the domain too: when the org leaves, its domain's tombstones
  leave with the org's data export. If another org later proves the same domain, it must
  not be able to receive mail for those addresses (open question 6).
- DNS, DKIM and reputation for a custom domain are the customer's, as in self-hosted.

## The org boundary

From T's downstream contract (T "Tokens and suite contracts"), applied to masked and
plus addresses:

| T requirement | Today in KyPost | Hosted change for masked/plus |
|---|---|---|
| Refuse a token without `org_id` | No org claim is read; accounts bind by `(issuer, sub)` (`GetBySSOSubIssuer`, `backend/internal/api/sync_handlers.go:133`) | KyPost tenancy work (D6) |
| Bind accounts to `(sub, org_id)` | Not bound | KyPost tenancy work |
| Take the org only from a verified token, a signed webhook or a suite answer | The signed sync webhook has no org (`sync_handlers.go:29-80`) | KyPost tenancy work. T adds `org_id` to the signed body |
| Key every row by `org_id` | Masked records are in per-user `config.yaml`; ledger addresses have no org | D8: ledger rows carry `org_id`; `POST/PUT/DELETE /api/addresses/masked` resolve the caller's org from the session, never from the request |
| Stop serving an org that is not `active` | n/a | Creation refused; receiving answers per open question 2; sending refused (outbox admission) |
| Enforce quotas from `/api/suite/orgs` | Install-wide constants | D7 |

Masked-specific rules on top:

- **Who can see a masked address.** Only its owner. Staff can see that one exists (for
  abuse handling) through the audited staff route. No other member of the same
  household org sees it, because masked addresses are personal. T's per-org scoping
  is necessary but not sufficient here; the owner check stays.
- **Admin routes.** `PUT /api/admin/masked-domains` becomes staff-only and accepts only
  platform-owned domains in hosted mode. Release and reassign keep their `409` for masked
  addresses (`native_address_handlers.go:84`), and additionally refuse a cross-org
  target mailbox.

## What changes in KyPost's code, and what stays

Changes, hosted mode only, after KyPost tenancy:

1. `randomMaskedAddress` takes a format per domain class (D3). `masked_addresses.go:66`.
2. Masked records move into the ledger or a per-account table with `org_id` (D8).
   `maskedHeld` and `sweepMaskedAddresses` read that instead of every `config.yaml`.
   `config.go:210-245` remains for single-tenant installs.
3. The limit checks read org quotas (D7). `masked_addresses.go:358-374`.
4. Org deletion writes tombstones (D5). `sso.LifecycleStore` ledger code.
5. The masked-domain admin route is restricted to staff and platform domains.
   `masked_addresses.go:103`, `server.go:735-736`.
6. Receiving checks org status: suspended means a temporary refusal (open question 2).
   `receiving.go:482-491` and `receiving_cfcontinuous.go:411`.
7. A per-source refused-recipient budget in the receiving path (abuse table).

Stays single-tenant until KyPost's tenancy work: everything else. That includes accounts,
admin UI, backups and restore (whole-install, like KyIdentity's, T "Replicas, Postgres
and backups"), quarantine, sender blocks, the relay profile (one per install, which suits
one relay domain) and the receiving profile. There is no KyPost tenancy spec in this
repository yet (`docs/` has none; unverified whether one exists elsewhere).

## Threat model

| Attacker | Goal | Defence |
|---|---|---|
| Spammer on the internet | Find live masked addresses on the shared relay domain | D3 entropy. Identical refusal for unknown, paused, expired and deleted (`config.go:235-245`, PR #675 evidence). Per-IP limits, plus a refused-recipient budget |
| Spammer who learned one masked address | Keep mailing it | Owner pauses or deletes it; refusal is immediate on both profiles (`TestNativeReceivingMaskedStates`) |
| Customer in org A | Read or control org B's masked addresses | Org-scoped records (D8) under KyPost tenancy; owner check on every masked route; cross-org ID answers like a missing one |
| Customer in org A | Receive org B's future mail by getting a deleted address reissued | Never-delete ledger and tombstones that survive org deletion (D5) |
| Customer | Unmask another customer: link a masked address to its owner's primary | No header names the primary (`TestMaskedSendLeaksNoPrimaryAddress`); DKIM has no `i=` (`TestDKIMSignatureCarriesNoIdentity`); masked and primaries on separate domains (D2) |
| Abusive customer | Burn the shared relay domain's reputation | Per-org creation and send quotas (D7), circuit breaker, staff suspension, DMARC reports to staff |
| Customer | Farm addresses for sign-up fraud | Per-user, per-org and platform caps (D7) |
| Relay provider or Cloudflare | Learn every hosted masked address | Inherent to the chosen profile (Cloudflare's routing table). Maddy avoids it at the cost of exposing our MX. Open question 5 |
| Staff, or a stolen staff account | Read who owns a masked address | Audited staff route only; T's staff controls (passkeys, audit in the org's log) |
| Holder of the relay domain's DKIM key (direct mode) | Forge masked mail for every customer | Prefer relay mode; rotate per `docs/DIRECT_DELIVERY.md` |

## Residual risks

- **Liveness still leaks.** A `250` versus `550` on RCPT is how SMTP works. D3 makes
  hitting a live address rare; it does not make probing impossible.
- **Shared reputation.** One abusive customer can still get the relay domain filtered
  for a while before quotas or staff catch it.
- **Provider visibility.** With Cloudflare, one provider sees the full list of hosted
  masked addresses.
- **Tombstones grow forever.** The address list is permanent by design. With D3 the space
  is effectively unexhaustable, but storage grows with every address ever made.
- **Masked plus hosted primary in one household.** Other household members never see
  each other's masked addresses, but staff can.
- **Unverified scale.** The minute sweep and per-user file reads are not measured at
  hosted scale (D8 removes them).

## Open questions, with recommendations

1. **More than one hosted KyPost install (sharding).** One ledger no longer covers
   uniqueness. *Recommend:* stay on one install for v1. If sharding comes, partition the
   relay domain by subdomain per shard (`s1.relay.<platform>`), so each shard's ledger is
   authoritative for its own subdomain and no cross-shard allocator is needed.
2. **Mail to a suspended org's masked addresses.** *Recommend:* temporary refusal (`451`)
   while `suspended`, so senders retry, then the same `550` as unknown from
   `deletion_scheduled` on. Payment suspensions are often resolved within days.
3. **Disposable lifetimes on the relay domain.** *Recommend:* keep 24h/7d/30d; an expired
   address becomes a tombstone like a deleted one (today's sweep behaviour).
4. **What a tombstone keeps after org deletion.** *Recommend:* an HMAC of the address under
   a platform key, not the address itself. The conflict check hashes the candidate first.
   This keeps never-reuse without retaining the customer's address list. Cost: a restore
   needs the same key.
5. **Cloudflare versus Maddy for the relay domain.** *Recommend:* Maddy (or another
   receiver we run) for the relay domain, because the Cloudflare routing table discloses
   every customer's masked addresses to one third party. Revisit if inbound volume makes
   running port 25 impractical.
6. **Custom domains changing hands.** *Recommend:* refuse a new org's proof for a domain
   that still has tombstones from a deleted org, for a cooling-off period (for example 90
   days), then allow it with the old tombstones still refusing.
7. **Isolated platforms on the shared relay domain.** Should a business be able to use
   the platform relay domain instead of its own? *Recommend:* no in v1. It would need a
   cross-install allocator (question 1) for a feature businesses can get on their own
   domain.
8. **Provider feedback.** KyPost does not ingest provider bounces or complaints
   (unverified that nothing exists). *Recommend:* build a minimal complaint-feedback path
   for the relay domain before hosted masked addresses go live; it is the main reputation
   signal.

## Verification for a later implementation

- Self-hosted unchanged: the existing tests (`TestMaskedAddressLifecycle`,
  `TestNativeReceivingMaskedStates`, `TestCloudflareRoutesPlusAndMasked`,
  `TestMaskedSendLeaksNoPrimaryAddress`) pass untouched, and the native-mail harness
  (`docs/DEVELOPMENT.md#native-mail-harness`) checks 3 and 4 still pass.
- Hosted: a cross-org matrix for every masked route, a tombstone survival test across
  org deletion, a quota test per org, and a harness run with KyIdentity in multi-tenant
  mode once T is built.
