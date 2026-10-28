/**
 * Shared push-relay logic used by both Cloudflare Workers: the FCM relay
 * (worker/) and the APNs relay (worker-apns/). Everything below is identical
 * between them — self-registration, per-minute rate limiting,
 * usage analytics, and small crypto/HTTP helpers.
 *
 * Each worker keeps its own `Env` (extending CommonEnv with its
 * provider-specific secrets) and its own `handleSend()`/`configured` pieces,
 * which createRelayFetchHandler below assembles into each worker's `fetch`.
 */

import type { KeyRow, RelayCoordinator } from "./relay-coordinator";

/** Cloudflare native rate-limiting binding (configured in wrangler.toml). */
export interface RateLimitBinding {
  limit(options: { key: string }): Promise<{ success: boolean }>;
}

/** Cloudflare Workers Analytics Engine binding (subset we use). */
export interface AnalyticsEngineDatasetLike {
  writeDataPoint(event: { indexes?: string[]; blobs?: (string | null)[]; doubles?: number[] }): void;
}

/**
 * Minimal structural Env shared by both workers. Each worker's own `Env`
 * extends this with its provider-specific secrets (FCM_* / APNS_*) and its
 * own KV cache binding (OAUTH_CACHE / APNS_TOKEN_CACHE).
 */
export interface CommonEnv {
  /**
   * Key records from before the registry (see the key-records section). Read
   * only, as the fallback for a key not yet copied into the registry; nothing
   * writes here any more.
   */
  API_KEYS: KVNamespace;
  /**
   * Minute limit is ENFORCED by the PUSH_RATE_LIMITER binding (simple.limit in
   * wrangler.toml). This var is display-only (/health + 429 body) and must be kept
   * equal to that binding's limit.
   *
   * Hour/day rolling tiers are deliberately absent — see the single-tier note
   * above checkMinuteLimit, whose fail-closed behaviour depends on it.
   */
  RATE_LIMIT_PER_MINUTE?: string; // display only; default 10
  /**
   * Relay-wide ceiling on sends per UTC day, ENFORCED via the coordinator's
   * spendDailyBudget. Unset means unmetered, which is what a deployment that
   * never configures it keeps getting; "0" is a real limit of zero.
   *
   * This is the aggregate tier the single-tier note above says is missing. It
   * is not per-key on purpose — see checkDailyBudget.
   */
  RELAY_DAILY_BUDGET?: string;
  /** Public self-registration (`POST /register`). "true" opens it; default closed. */
  REGISTRATION_ENABLED?: string;
  /**
   * Opt out of fail-closed rate limiting when the limiter bindings are
   * unavailable — local dev only. Never set this in a deployed Worker.
   */
  RATELIMIT_FAIL_OPEN?: string;
  /** Minute-tier rate limiter (native binding, no KV writes). */
  PUSH_RATE_LIMITER?: RateLimitBinding;
  /**
   * Per-IP minute-tier limiter for POST /register (native binding, no KV writes).
   * The "one active key per IP" dedup in handleRegister limits only *concurrent*
   * keys per IP, not how fast an IP can churn through new ones, so this bounds
   * minting many short-lived permanent keys from one address before rotating IPs.
   */
  REGISTER_RATE_LIMITER?: RateLimitBinding;
  /** Per-key usage counters, offloaded off the KV write path. */
  USAGE_ANALYTICS?: AnalyticsEngineDatasetLike;
  /**
   * Strongly-consistent store for what KV cannot hold: device-token pinning, the
   * key records (with one active key per IP), and the daily budget. See
   * RelayCoordinator. Optional in the type only so the binding can be reported
   * missing rather than crashing; every call site fails CLOSED without it.
   */
  RELAY_COORDINATOR?: DurableObjectNamespace<RelayCoordinator>;
}

export interface ApiKeyRecord {
  id: string;
  label: string;
  enabled: boolean;
  createdAt: string;
  /** ISO timestamp after which the key is rejected; null/absent = never expires. */
  expiresAt?: string | null;
  /**
   * How the key was issued. Every key minted now is "self" (via /register);
   * "admin" only appears on records left by the admin API that used to exist.
   */
  source?: "admin" | "self";
  /** Client IP captured at self-registration, for auditing abuse. */
  registeredIp?: string | null;
}

// Legacy KV layout, read only. Keys live in the registry now; these are its
// fallback, and the seed for a token instance that has never been touched.
export const KEY_PREFIX = "key:"; // key:<sha256(key)>      -> ApiKeyRecord
export const KEY_INDEX_PREFIX = "keyid:"; // keyid:<id>     -> <sha256(key)>
export const SUPERSEDED_PREFIX = "superseded:"; // superseded:<sha256(key)> -> "1", a key /register replaced
export const BOUND_TOKEN_PREFIX = "boundtoken:"; // boundtoken:<sha256(deviceToken)> -> keyId (first sender owns it)

export const DEFAULT_LIMIT_PER_MINUTE = 10;

/**
 * Max stored label length. A label is an operator-facing name for one server,
 * not a payload; without a bound, public /register lets anyone persist
 * megabyte-sized strings into KV and into every operator's key listing.
 */
export const MAX_LABEL_LENGTH = 64;

// ---- request body bounds ----------------------------------------------------
//
// Both endpoints that take a body parse attacker-supplied JSON: /register takes
// one short label, /send takes one notification. Without a ceiling read
// BEFORE the parse, `await request.json()` allocates whatever was sent — the
// per-minute limiter caps how many requests a key may make, not how large each
// one is, so one key at its quota could still make the relay materialize
// hundreds of megabytes a minute and forward them upstream.
//
// The field bounds below are the second half: a body that fits still must not
// carry a 12 KB "device token" into a provider request.

/** Ceiling on a POST /send body. */
export const MAX_SEND_BODY_BYTES = 16 * 1024;
/** Ceiling on a POST /register body (one label). */
export const MAX_JSON_BODY_BYTES = 4 * 1024;

/**
 * Field bounds for /send. Real device tokens are ~64 (APNs) to ~255 (FCM)
 * characters, so a longer one is not a token that got clipped, it is not a
 * token — hence rejected rather than clamped. Text fields are clamped instead:
 * a title is a sender and a body is a subject, both of which arrive from
 * whoever emailed the self-hoster, and refusing to deliver a notification
 * because a stranger sent a long subject line is a worse failure than
 * delivering a clipped one. Same reasoning as MAX_LABEL_LENGTH.
 */
export const MAX_TOKEN_LENGTH = 512;
export const MAX_TITLE_LENGTH = 256;
export const MAX_NOTIFICATION_BODY_LENGTH = 1024;
export const MAX_DATA_ENTRIES = 16;
export const MAX_DATA_KEY_LENGTH = 64;
export const MAX_DATA_VALUE_LENGTH = 1024;

/** Returns the canonical form of a device token, or null if it is not one. */
export type TokenFormat = (token: string) => string | null;

// The pin is keyed on sha256(token), so two spellings of one device are two
// unpinned claims. Each format admits exactly one spelling per device: no
// `/`, `.`, `%`, `?` or `#` to alias a URL path, no case or padding variants.

/** APNs: hex, 32 bytes today; Apple says the length may grow. Lowercase is canonical. */
export const apnsToken: TokenFormat = (token) => {
  const lower = token.toLowerCase();
  return /^[0-9a-f]{64,200}$/.test(lower) ? lower : null;
};

/** FCM: opaque and case-sensitive, so only the alphabet is checked; length is MAX_TOKEN_LENGTH. */
export const fcmToken: TokenFormat = (token) => (/^[A-Za-z0-9_:-]+$/.test(token) ? token : null);

/**
 * The one way a caught value becomes log text.
 *
 * A `catch` binding is `unknown` and JavaScript lets anything be thrown, so
 * `(err as Error).message` is a lie the compiler accepts: on `throw null` it
 * throws a TypeError of its own, out of the handler that exists to keep the
 * failure contained. Every one of these sits on a fail-closed path — the 502
 * for a dead provider, the 429 for a throwing rate limiter — so the replacement
 * failure is the generic 500 from the outer router, and the log line records
 * the logging bug instead of the outage.
 *
 * Total by construction: `String()` accepts every value, including symbols.
 */
export function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

// ---- small helpers ---------------------------------------------------------

export interface RequestContext<TEnv extends CommonEnv = CommonEnv> {
  env: TEnv;
  ctx: ExecutionContext;
  requestId: string;
  log: (fields: Record<string, unknown>) => void;
}

export function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

/** Error response carrying the request id so callers can correlate with logs. */
export function fail(
  rc: RequestContext,
  status: number,
  message: string,
  extra?: Record<string, unknown>,
): Response {
  return json({ error: message, requestId: rc.requestId, ...(extra ?? {}) }, status);
}

/**
 * The one answer every failed delivery gets, whatever the provider said.
 *
 * FCM and APNs error bodies describe OUR project: the Firebase project id, the
 * APNs topic, the service account, quota and routing state, and whatever
 * diagnostic Google or Apple adds next. That text used to be interpolated
 * straight into the 502 body, so any holder of any per-server API key — which,
 * with public registration open, is anyone — could read the relay's upstream
 * configuration by sending deliberately broken sends and reading the errors.
 *
 * The caller gets a stable, coarse failure plus the request id, which is what
 * they can act on (retry, or quote the id to the operator).
 *
 * The provider's status and its reason clipped to 200 characters DO go to the
 * structured log, at each worker's `send.fcm_failed` / `send.apns_failed`. That
 * is a deliberate, bounded exception to "no upstream bodies in logs" — since
 * isStaleResponse stopped retiring devices on the 400s that name a token, that
 * log line is the only place an operator learns the relay is misconfigured
 * rather than the phones being dead. Its terms are written down in
 * push-relay-shared/AGENTS.md; the distinction this function enforces is
 * caller-facing versus operator-facing, not logged versus unlogged.
 */
export function failDelivery(rc: RequestContext): Response {
  return fail(rc, 502, "push delivery failed");
}

export function bearer(request: Request): string {
  const header = request.headers.get("Authorization") ?? "";
  const match = /^Bearer\s+(.+)$/i.exec(header.trim());
  return match ? match[1].trim() : "";
}

/**
 * Buckets a client IP for anti-abuse keying: IPv4 unchanged, IPv6 collapsed to
 * its /64 prefix. A single IPv6 allocation is typically a /64 (2^64 addresses),
 * so keying on the full 128-bit address lets an attacker mint unlimited keys by
 * sourcing each request from a fresh address in their own allocation.
 *
 * Returns "" for anything that is not a syntactically valid address — including
 * an empty string, a placeholder like "unknown", and the IPv4-mapped `::ffff:`
 * form, whose /64 is 0:0:0:0 for every client on the internet and so is not a
 * bucket at all. handleRegister treats "" as "no client address" and refuses to
 * mint, which is the only safe answer for controls that are keyed on this.
 */
export function ipBucket(ip: string): string {
  const trimmed = ip.trim();
  if (!trimmed.includes(":")) {
    return /^\d{1,3}(\.\d{1,3}){3}$/.test(trimmed) && trimmed.split(".").every((o) => Number(o) <= 255)
      ? trimmed
      : "";
  }
  // Strip any zone id and expand "::" to reconstruct the first four hextets.
  const addr = trimmed.split("%")[0];
  const [head, tail, extra] = addr.split("::");
  const headGroups = head ? head.split(":").filter((g) => g !== "") : [];
  const tailGroups = tail ? tail.split(":").filter((g) => g !== "") : [];
  const compressed = tail !== undefined;
  const written = headGroups.length + tailGroups.length;
  const groups = compressed
    ? [...headGroups, ...Array(Math.max(0, 8 - written)).fill("0"), ...tailGroups]
    : headGroups;
  // Three checks, and all three have let something through on their own:
  //
  //   - one "::" at most ("1::2::3" is not an address);
  //   - the right number of written hextets — exactly 8 uncompressed, at most 7
  //     when "::" is present, since it stands for one or more omitted groups.
  //     Without this, "2001:db8:1" zero-padded into a perfectly plausible
  //     bucket despite being three hextets and no compression marker;
  //   - every group is hex, not just the four that make the prefix. Checking
  //     only the prefix let "::ffff:garbage" through as a bucket it then shared
  //     with everything else that pads to zeros.
  if (extra !== undefined || (compressed ? written > 7 : written !== 8)) {
    return "";
  }
  if (!groups.every((g) => /^[0-9a-f]{1,4}$/i.test(g))) {
    return "";
  }
  return [0, 1, 2, 3].map((i) => (groups[i] ?? "0").toLowerCase()).join(":") + "::/64";
}

export async function sha256Hex(input: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(input));
  return [...new Uint8Array(digest)].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export function randomToken(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  return [...bytes].map((b) => b.toString(16).padStart(2, "0")).join("");
}

export function isExpired(record: ApiKeyRecord, now: number): boolean {
  if (!record.expiresAt) {
    return false;
  }
  const at = Date.parse(record.expiresAt);
  return Number.isFinite(at) && at <= now;
}

// ---- device-token pinning ---------------------------------------------------
//
// Closes the open-relay gap left by public self-registration (handleRegister):
// without it, any key holder could /send to any device token, spoofing push
// notifications at strangers. The first key to successfully deliver to a token
// claims it, and every later /send to that token must come from the same key. A
// claim is released for reclaiming once the owning key is revoked/expired/
// deleted, so key rotation never permanently orphans a legitimate device.
//
// The claim itself lives in RelayCoordinator, a Durable Object, because "is this
// token free?" followed by "take it" against eventually consistent KV is not one
// decision — two keys racing the first send to a token both read "free" and both
// were let through, which is the spoofing this section exists to prevent.

/**
 * Resolves one coordinator instance by name. Naming the instance after the
 * thing being claimed is what serializes the racers: every request for one
 * token lands on the same object.
 */
function coordinatorFor(env: CommonEnv, name: string): DurableObjectStub<RelayCoordinator> | null {
  const ns = env.RELAY_COORDINATOR;
  if (!ns) {
    return null;
  }
  return ns.get(ns.idFromName(name));
}

export function tokenCoordinator(env: CommonEnv, tokenHash: string): DurableObjectStub<RelayCoordinator> | null {
  return coordinatorFor(env, "token:" + tokenHash);
}

/** Whether keyId still names a key that may hold a claim. */
async function keyIsActive(env: CommonEnv, keyId: string): Promise<boolean> {
  const registry = keyRegistry(env);
  if (!registry) {
    return true; // cannot tell, so no takeover
  }
  const row =
    (await registry.findKey({ id: keyId })) ??
    (await importFromKv(env, registry, await env.API_KEYS.get(KEY_INDEX_PREFIX + keyId)));
  return row !== null && row.status === "active" && !isExpired(recordOf(row), Date.now());
}

/**
 * How long a claim is protected from takeover. For a key not yet copied into
 * the registry, keyIsActive still answers out of KV, which converges globally in
 * about a minute: a key written seconds ago can read as ABSENT at a PoP that has
 * not seen the write. Registry answers are consistent, so this matters only
 * while the KV fallback exists; it delays a legitimate takeover of a token whose
 * owning key went away within the last minute, nothing more.
 */
const CLAIM_TAKEOVER_GRACE_MS = 60_000;

/**
 * Ends one send's use of a token claim: `delivered` true after the push was
 * actually accepted by the provider, false on every failure path. Mandatory
 * after any allowed claimTokenForSend — the coordinator counts sends in and out,
 * and a claim under which nothing ever delivered is released when the last one
 * settles. See RelayCoordinator.settleToken for why the decision cannot be made
 * out here.
 */
export async function settleToken(
  rc: RequestContext,
  token: string,
  keyId: string,
  delivered: boolean,
): Promise<void> {
  const tokenHash = await sha256Hex(token);
  const stub = tokenCoordinator(rc.env, tokenHash);
  if (!stub) {
    return;
  }
  await stub.settleToken(keyId, delivered);
}

export type ClaimResult =
  | { allowed: true }
  // `status`/`logReason` travel with the denial so a misconfigured relay is not
  // reported to the caller as "that token belongs to someone else" — a 403 tells
  // a self-hoster their device is spoken for and to stop retrying, which is the
  // wrong answer to an outage on our side.
  | { allowed: false; reason: string; status: number; logReason: string };

/**
 * Claims token for keyId BEFORE delivery — claiming afterwards would let a
 * first-sender deliver a spoofed push ahead of the legitimate owner's claim.
 * Every allowed claim MUST be closed out with settleToken, which is what
 * releases a claim whose send turned out never to deliver.
 *
 * The claim is a single serialized turn inside the token's RelayCoordinator, so
 * two keys racing the first send to one token can no longer both be told the
 * token is free. The one question it cannot answer itself — is the current
 * owner's key still active, which decides whether a rotated key's tokens can be
 * re-claimed — is the registry's, asked out here and applied back as a
 * compare-and-swap, so a re-claim by the
 * legitimate owner in the meantime beats the takeover rather than being
 * overwritten by it.
 *
 * Fails CLOSED when the binding is missing, for the same reason checkMinuteLimit
 * does: "the coordinator is misconfigured" and "there is no coordinator" are
 * indistinguishable from outside, and falling back to the KV path would restore
 * the exact race this exists to remove, silently.
 */
export async function claimTokenForSend(rc: RequestContext, token: string, keyId: string): Promise<ClaimResult> {
  const { env } = rc;
  const tokenHash = await sha256Hex(token);
  const stub = tokenCoordinator(env, tokenHash);
  if (!stub) {
    rc.log({ level: "error", event: "tokenclaim.binding_missing" });
    return {
      allowed: false,
      reason: "token ownership coordinator unavailable",
      status: 503,
      logReason: "coordinator_binding_missing",
    };
  }

  // Pre-Durable-Object claims live in KV; hand the recorded owner over so this
  // token's instance adopts it the first time it is touched, instead of treating
  // every already-owned token as free. Ignored on every later call.
  const legacyOwner = await env.API_KEYS.get(BOUND_TOKEN_PREFIX + tokenHash);

  const first = await stub.claimToken({ keyId, legacyOwner });
  if (first.owner === keyId) {
    return { allowed: true };
  }

  const denied = (logReason: string): ClaimResult => ({
    allowed: false,
    reason: "token is already bound to a different active api key",
    status: 403,
    logReason,
  });
  // Ask the eventually consistent question only about a claim old enough for the
  // answer to be trustworthy — see CLAIM_TAKEOVER_GRACE_MS.
  if (Date.now() - first.claimedAt < CLAIM_TAKEOVER_GRACE_MS) {
    return denied("token_claim_too_recent");
  }
  if (await keyIsActive(env, first.owner)) {
    return denied("token_bound_to_other_key");
  }

  const takeover = await stub.claimToken({ keyId, takeoverFrom: first.owner });
  if (takeover.owner !== keyId) {
    return denied("token_bound_to_other_key"); // re-claimed between the two calls
  }
  return { allowed: true };
}

// ---- per-key rate limits ---------------------------------------------------

/**
 * Resolve a limit var:
 *
 *   unset / blank   -> fallback
 *   "0" or negative -> 0 (negatives clamp up, they are not a smaller limit)
 *   unparseable     -> fallback
 *
 * Unparseable resolves to the FALLBACK, not to 0: a typo in a deployment
 * variable must not read as "no limit configured", which is the
 * fail-open-on-misconfiguration checkMinuteLimit below exists to refuse.
 */
export function resolveLimit(raw: string | undefined, fallback: number): number {
  if (raw === undefined || raw.trim() === "") {
    return fallback;
  }
  const parsed = Number.parseInt(raw.trim(), 10);
  return Number.isFinite(parsed) ? Math.max(0, parsed) : fallback;
}

// Single-tier PER-KEY rate limiting: a decision, not a gap. Stated here rather
// than as a TODO because checkMinuteLimit's fail-closed behaviour depends on
// the minute tier being the only PER-KEY tier.
//
// Enforced per key: a per-minute cap via the native rate-limiting bindings
// (PUSH_RATE_LIMITER per key, REGISTER_RATE_LIMITER per IP). No KV writes on an
// accepted send.
//
// Not enforced per key: rolling hour and day caps. They required a KV
// read-modify-write per send, which capped the free tier at ~1,000 pushes/day —
// the limiter became the outage. They are not coming back in that form.
//
// The cost: a caller who stays under the per-minute cap can sustain it
// indefinitely, so the minute limit bounds burst rate but not daily volume
// against someone else's infrastructure — the relay operator's Cloudflare
// account first, and their provider's send-rate quota after it. That residual
// is why the fail-closed behaviour below is not negotiable.
//
// The daily half of that residual is now covered — by checkDailyBudget, built
// the way this note prescribed (a Durable Object counter, no KV write
// pressure) and AGGREGATE rather than per-key, since a per-key day cap is
// worth nothing when registration mints keys on demand. It is opt-in via
// RELAY_DAILY_BUDGET and unset by default, so the paragraph above still
// describes an unconfigured relay exactly. What has NOT changed is the
// reasoning at this function's call sites: the minute tier is still the only
// per-key tier, so failing it closed is still not negotiable.

/**
 * Minute-tier check via a native rate-limiting binding (no KV writes). Returns
 * true when allowed, and fails closed on both a missing binding and a throwing
 * one. Shared by the per-key send limiter (PUSH_RATE_LIMITER) and the per-IP
 * registration limiter (REGISTER_RATE_LIMITER); key is whatever the caller wants
 * to bucket on. Local dev without binding support opts out explicitly via
 * RATELIMIT_FAIL_OPEN.
 */
export async function checkMinuteLimit(
  limiter: RateLimitBinding | undefined,
  rc: RequestContext,
  key: string,
): Promise<boolean> {
  if (!limiter || typeof limiter.limit !== "function") {
    // Fail CLOSED on a missing binding. "The limiter is misconfigured" and
    // "there is no limiter" are indistinguishable from outside, and the minute
    // tier is the only tier enforced (see the note above) — so failing
    // open here removed rate limiting entirely, silently, from a deployment
    // whose wrangler.toml had drifted. Local dev without binding support opts
    // out explicitly via RATELIMIT_FAIL_OPEN.
    if ((rc.env.RATELIMIT_FAIL_OPEN ?? "").trim().toLowerCase() === "true") {
      return true;
    }
    rc.log({ level: "error", event: "ratelimit.binding_missing", key });
    return false;
  }
  try {
    const { success } = await limiter.limit({ key });
    return success;
  } catch (err) {
    // Fail closed, same as a missing binding. Returning true would treat a throwing
    // binding as an outage that should not take delivery down with it, but the
    // minute tier is the only tier enforced (see the note above), so this branch is
    // the difference between a rate-limited relay and an unbounded send primitive
    // pointed at someone else's infrastructure, traced only by this log line.
    //
    // Callers surface it as 429 with a Retry-After: the request is refused not
    // because the caller misbehaved but because the relay cannot currently tell
    // whether they did.
    rc.log({ level: "error", event: "ratelimit.binding_error", error: errorMessage(err) });
    return false;
  }
}

/**
 * The Durable Object instance holding the relay-wide daily budget. One fixed
 * name, because the budget is one shared pool — see spendDailyBudget.
 */
const BUDGET_INSTANCE = "relay-daily-budget";

/**
 * Seconds until the budget resets, for Retry-After. The counter rolls at the
 * UTC day boundary, so this is a real time at which the caller's next attempt
 * can succeed rather than a guess — which is what makes it worth sending
 * instead of a fixed 60.
 */
export function secondsUntilUTCMidnight(now: number = Date.now()): number {
  const midnight = Date.UTC(
    new Date(now).getUTCFullYear(),
    new Date(now).getUTCMonth(),
    new Date(now).getUTCDate() + 1,
  );
  return Math.max(1, Math.ceil((midnight - now) / 1000));
}

/**
 * Draw one send from the relay-wide daily budget.
 *
 * This is the upper tier the single-tier note above asks for, built the way
 * that note specifies: a Durable Object counter rather than a KV
 * read-modify-write, so it does not reintroduce the write pressure that made
 * the previous rolling limits the outage.
 *
 * It is AGGREGATE, not per-key. The per-minute limiters bucket per key and per
 * registering IP, and with public registration open neither is a scarce
 * resource: an actor who wants a second bucket registers a second key from a
 * second address. So the ceiling has to be counted together, or extra keys buy
 * extra headroom and the limit means nothing.
 *
 * What that ceiling protects is worth naming precisely, because it is easy to
 * assume it is a bill and it is not. FCM and APNs are free: there is no
 * provider invoice, and the send quotas they do impose are per-project RATE
 * limits rather than a daily cap. The resources actually being spent are the
 * operator's — the Cloudflare account underneath, where free-plan daily limits
 * on KV and Durable Object operations refuse further operations outright until
 * 00:00 UTC, and on a paid plan become the invoice — plus the provider's
 * goodwill, since an open relay driven hard is indistinguishable from an abuse
 * source.
 *
 * This counter is blind to every one of those Cloudflare limits. It bounds what
 * the relay chooses to send; it cannot see what Cloudflare is willing to serve.
 * Sizing it below the plan's own ceilings is the operator's job.
 *
 * Unset RELAY_DAILY_BUDGET means unmetered, which is what every existing
 * deployment gets. An operator opts into the ceiling; nobody wakes up to find
 * their working relay capped. "0" is a real limit of zero (a closed relay),
 * not an unset value — the inversion resolveLimit already refuses elsewhere.
 *
 * Fails CLOSED when configured but uncountable, for the same reason
 * checkMinuteLimit does: once a budget exists it is the only thing bounding
 * daily volume, and "the binding drifted" is indistinguishable from "there is
 * no binding" from out here.
 */
export async function checkDailyBudget(
  rc: RequestContext,
  now: number = Date.now(),
): Promise<{ allowed: boolean; used: number; limit: number }> {
  const raw = (rc.env.RELAY_DAILY_BUDGET ?? "").trim();
  if (raw === "") {
    return { allowed: true, used: 0, limit: 0 };
  }
  const limit = resolveLimit(raw, 0);

  const binding = rc.env.RELAY_COORDINATOR;
  if (!binding || typeof binding.idFromName !== "function") {
    rc.log({ level: "error", event: "budget.binding_missing" });
    return { allowed: false, used: 0, limit };
  }

  try {
    const stub = binding.get(binding.idFromName(BUDGET_INSTANCE));
    const result = await stub.spendDailyBudget(limit, now);
    if (!result.allowed) {
      rc.log({ level: "warn", event: "budget.exhausted", used: result.used, limit, day: result.day });
    }
    return { allowed: result.allowed, used: result.used, limit };
  } catch (err) {
    rc.log({ level: "error", event: "budget.error", error: errorMessage(err) });
    return { allowed: false, used: 0, limit };
  }
}

/**
 * Give back a draw taken by a send that never reached the provider.
 *
 * checkDailyBudget draws before dispatch, because the counter has to be taken in
 * the coordinator's serialized turn and the outcome is not known until after.
 * That is right for the common case and wrong for the case where no provider
 * request is ever made: a bad APNS_KEY_ID or FCM service account fails every
 * send before it leaves Cloudflare, and un-refunded those failures spend the
 * whole day's budget on zero delivery — so the relay stays refused until UTC
 * midnight even after the credential is fixed.
 *
 * Callers refund ONLY on a result that proves no provider request was made
 * (`reachedProvider: false`) or on a thrown send. A provider that answered — a
 * dead token, a rejected token, a 5xx — spent the quota this budget bounds, and
 * refunding those would make invented device tokens free.
 *
 * Best-effort and never throws: a failed refund overcounts the day by one, which
 * is the safe direction, and the request it belongs to has already failed.
 */
export async function refundDailyBudget(rc: RequestContext, now: number = Date.now()): Promise<void> {
  if ((rc.env.RELAY_DAILY_BUDGET ?? "").trim() === "") {
    return;
  }
  const binding = rc.env.RELAY_COORDINATOR;
  if (!binding || typeof binding.idFromName !== "function") {
    return;
  }
  try {
    await binding.get(binding.idFromName(BUDGET_INSTANCE)).refundDailyBudget(now);
  } catch (err) {
    rc.log({ level: "error", event: "budget.refund_failed", error: errorMessage(err) });
  }
}

/**
 * Record one accepted send to Analytics Engine (off the KV write path). Query
 * lifetime totals per key later via the WAE SQL API. Best-effort: never throws.
 */
export function recordUsageAnalytics(env: CommonEnv, record: ApiKeyRecord): void {
  const wae = env.USAGE_ANALYTICS;
  if (!wae) {
    return;
  }
  try {
    wae.writeDataPoint({
      indexes: [record.id],
      blobs: [record.id, record.label, record.source ?? "self"],
      doubles: [1],
    });
  } catch {
    // analytics is best-effort; a send must never fail on it.
  }
}

// ---- key records --------------------------------------------------------------
//
// Every key lives in one RelayCoordinator instance, KEY_REGISTRY, in a SQLite
// table (see RelayCoordinator.registerKey). Records used to live in API_KEYS
// (KV): a registration cost four KV writes, the free plan allows 1,000 a day,
// and on 2026-10-26 test servers registering against the production relays spent
// them and took Android push down. A registry write is one row in a Durable
// Object, whose free-plan allowance is 100,000 rows a day.
//
// Each relay Worker has its own Durable Object namespace, so each keeps its own
// keys. In KV the FCM relays of two products shared one namespace, and with it
// one active key per IP across both.
//
// Migration is copy-on-use. The registry is asked first; a key it has never seen
// is looked up in KV and copied in (RelayCoordinator.importKey), so the
// registry's answer is final from the key's first use on. Nothing writes KV.
// Once no key has been copied for a while, the KV reads can go.

/** The registry instance's name, which is also how Data Studio finds it. */
export const KEY_REGISTRY = "relay-keys";

export function keyRegistry(env: CommonEnv): DurableObjectStub<RelayCoordinator> | null {
  return coordinatorFor(env, KEY_REGISTRY);
}

/**
 * What /send knows about a presented key. `unknown` means neither the registry
 * nor KV has ever heard of it: the relay lost it, and the server should
 * re-register. `revoked` is an operator's decision and stays refused.
 */
export type KeyLookup =
  | { status: "active"; record: ApiKeyRecord }
  | { status: "expired"; record: ApiKeyRecord }
  | { status: "superseded" | "revoked"; keyId?: string }
  | { status: "unknown" | "unavailable" };

// Active keys, briefly, so most sends skip the registry round trip. Per isolate
// and keyed by hash, never by the key itself. A key superseded or revoked
// elsewhere keeps working here for at most KEY_CACHE_TTL_MS, about the window
// KV's own propagation used to leave.
const KEY_CACHE_TTL_MS = 30_000;
const KEY_CACHE_MAX = 1024;
const keyCache = new Map<string, { record: ApiKeyRecord; until: number }>();

/** Drops every cached key answer. For tests; a deploy does the same. */
export function clearKeyCache(): void {
  keyCache.clear();
}

function recordOf(row: KeyRow): ApiKeyRecord {
  return {
    id: row.id,
    label: row.label,
    enabled: row.status === "active",
    createdAt: new Date(row.createdAt).toISOString(),
    expiresAt: row.expiresAt === null ? null : new Date(row.expiresAt).toISOString(),
    source: row.source === "admin" ? "admin" : "self",
    registeredIp: row.ip,
  };
}

/** Copies the KV record for `hash` into the registry; null when KV has none. */
async function importFromKv(
  env: CommonEnv,
  registry: DurableObjectStub<RelayCoordinator>,
  hash: string | null,
): Promise<KeyRow | null> {
  if (!hash) {
    return null;
  }
  const record = await env.API_KEYS.get<ApiKeyRecord>(KEY_PREFIX + hash, "json");
  if (!record || typeof record.id !== "string") {
    return null;
  }
  const expiresAt = record.expiresAt ? Date.parse(record.expiresAt) : NaN;
  return registry.importKey({
    hash,
    id: record.id,
    label: String(record.label ?? "").slice(0, MAX_LABEL_LENGTH),
    ip: record.registeredIp ?? null,
    createdAt: Date.parse(record.createdAt) || 0,
    expiresAt: Number.isFinite(expiresAt) ? expiresAt : null,
    source: record.source ?? "self",
    enabled: record.enabled === true,
  });
}

/** Resolves a presented key's hash: cache, then registry, then KV (copied in). */
export async function lookupKey(rc: RequestContext, hash: string, now: number = Date.now()): Promise<KeyLookup> {
  const cached = keyCache.get(hash);
  if (cached && cached.until > now && !isExpired(cached.record, now)) {
    return { status: "active", record: cached.record };
  }
  keyCache.delete(hash);

  const registry = keyRegistry(rc.env);
  if (!registry) {
    rc.log({ level: "error", event: "keys.binding_missing" });
    return { status: "unavailable" };
  }
  const row = (await registry.findKey({ hash })) ?? (await importFromKv(rc.env, registry, hash));
  if (!row) {
    // Keys /register replaced under KV left this tombstone; nothing writes it now.
    return (await rc.env.API_KEYS.get(SUPERSEDED_PREFIX + hash))
      ? { status: "superseded" }
      : { status: "unknown" };
  }
  if (row.status !== "active") {
    return { status: row.status, keyId: row.id };
  }
  const record = recordOf(row);
  if (isExpired(record, now)) {
    return { status: "expired", record };
  }
  if (keyCache.size >= KEY_CACHE_MAX) {
    keyCache.delete(keyCache.keys().next().value!);
  }
  keyCache.set(hash, { record, until: now + KEY_CACHE_TTL_MS });
  return { status: "active", record };
}

/**
 * The refusal for a key that is not active. The `code` tells a self-registered
 * server what to do: `key_superseded` and `key_unknown` mean register again;
 * no code (a revoked key) means stop, because an operator decided.
 */
export function denyKey(rc: RequestContext, key: Exclude<KeyLookup, { status: "active" }>): Response {
  const keyId = "record" in key ? key.record.id : "keyId" in key ? key.keyId : undefined;
  rc.log({ level: "warn", event: "send.denied", reason: key.status, keyId });
  switch (key.status) {
    case "unavailable":
      return fail(rc, 503, "key registry unavailable");
    case "superseded":
      return fail(rc, 401, "api key superseded", { code: "key_superseded" });
    case "unknown":
      return fail(rc, 401, "unknown api key", { code: "key_unknown" });
    case "expired":
      return fail(rc, 401, "api key expired", { expiresAt: key.record.expiresAt });
    case "revoked":
      return fail(rc, 401, "invalid api key");
  }
}

/** A refusal that a handler returns as-is; `ok: true` carries the parsed value. */
export type BodyResult<T> = { ok: true; value: T } | { ok: false; status: number; error: string };

const TOO_LARGE: BodyResult<never> = { ok: false, status: 413, error: "request body too large" };

/**
 * Read a request body with a hard byte ceiling, never holding more than that.
 *
 * Content-Length is checked first because it is free, but it is not trusted as
 * the bound: it is absent on a chunked body and it is a claim by the sender
 * either way. The stream is therefore counted as it arrives and cancelled the
 * moment it goes over, so the refusal costs `maxBytes`, not the body's size.
 */
export async function readBoundedBody(request: Request, maxBytes: number): Promise<BodyResult<string>> {
  const declared = Number(request.headers.get("Content-Length") ?? "");
  if (Number.isFinite(declared) && declared > maxBytes) {
    return TOO_LARGE;
  }
  const stream = request.body;
  if (!stream) {
    return { ok: true, value: "" };
  }
  const reader = stream.getReader();
  const chunks: Uint8Array[] = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      total += value.byteLength;
      if (total > maxBytes) {
        await reader.cancel();
        return TOO_LARGE;
      }
      chunks.push(value);
    }
  } catch {
    return { ok: false, status: 400, error: "could not read request body" };
  }
  const merged = new Uint8Array(total);
  let offset = 0;
  for (const chunk of chunks) {
    merged.set(chunk, offset);
    offset += chunk.byteLength;
  }
  return { ok: true, value: new TextDecoder().decode(merged) };
}

/**
 * Parse a bounded JSON object body, tolerating anything else. JSON.parse accepts
 * valid-but-non-object JSON (`null`, arrays, scalars), and reading a property off
 * `null` throws — a body of literal `null` turned these handlers into a 500. An
 * oversized body is NOT tolerated: it is refused before it is parsed, which is
 * the whole point of the ceiling.
 */
async function jsonObjectBody<T extends object>(
  request: Request,
  maxBytes = MAX_JSON_BODY_BYTES,
): Promise<BodyResult<Partial<T>>> {
  const body = await readBoundedBody(request, maxBytes);
  if (!body.ok) {
    return body;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(body.value || "{}");
  } catch {
    return { ok: true, value: {} };
  }
  return {
    ok: true,
    value: parsed && typeof parsed === "object" && !Array.isArray(parsed) ? (parsed as Partial<T>) : {},
  };
}

/** Clamp to `max` characters without splitting a surrogate pair in half. */
function clampString(value: string, max: number): string {
  if (value.length <= max) {
    return value;
  }
  const clipped = value.slice(0, max);
  const last = clipped.charCodeAt(max - 1);
  return last >= 0xd800 && last <= 0xdbff ? clipped.slice(0, max - 1) : clipped;
}

/**
 * The validated /send body, shaped for both providers: worker/'s FcmMessage and
 * worker-apns/'s PushMessage are the same four fields.
 */
export interface SendPayload {
  token: string;
  title: string;
  body: string;
  data: Record<string, string>;
}

/**
 * Read and validate a POST /send body against MAX_SEND_BODY_BYTES and the field
 * bounds above, returning a payload that is safe to hand to a provider.
 *
 * Types are checked, not coerced: `{"title": {...}}` used to become the string
 * "[object Object]" in a delivered notification, and `data` was forwarded to the
 * provider unexamined, so any JSON value at all could ride into an FCM/APNs
 * request under a key holder's quota. Unknown fields are ignored rather than
 * refused — the Go sender already sends a `platform` field this relay has never
 * read, and a new one must not become a delivery outage on the older worker.
 *
 * The token comes back in `format`'s canonical form, the only form that may be
 * hashed, pinned, charged for or sent.
 */
export async function readSendPayload(request: Request, format: TokenFormat): Promise<BodyResult<SendPayload>> {
  const body = await readBoundedBody(request, MAX_SEND_BODY_BYTES);
  if (!body.ok) {
    return body;
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(body.value);
  } catch {
    return { ok: false, status: 400, error: "invalid json body" };
  }
  if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
    return { ok: false, status: 400, error: "invalid json body" };
  }
  const raw = parsed as Record<string, unknown>;

  if (raw.token !== undefined && typeof raw.token !== "string") {
    return { ok: false, status: 400, error: "invalid token" };
  }
  const trimmed = ((raw.token as string | undefined) ?? "").trim();
  if (!trimmed) {
    return { ok: false, status: 400, error: "missing token" };
  }
  const token = trimmed.length > MAX_TOKEN_LENGTH ? null : format(trimmed);
  if (token === null) {
    return { ok: false, status: 400, error: "invalid token" };
  }

  for (const field of ["title", "body"] as const) {
    if (raw[field] !== undefined && typeof raw[field] !== "string") {
      return { ok: false, status: 400, error: `invalid ${field}` };
    }
  }

  const data: Record<string, string> = {};
  if (raw.data !== undefined && raw.data !== null) {
    if (typeof raw.data !== "object" || Array.isArray(raw.data)) {
      return { ok: false, status: 400, error: "invalid data" };
    }
    const entries = Object.entries(raw.data as Record<string, unknown>);
    if (entries.length > MAX_DATA_ENTRIES) {
      return { ok: false, status: 400, error: "too many data entries" };
    }
    for (const [key, value] of entries) {
      if (key.length > MAX_DATA_KEY_LENGTH) {
        return { ok: false, status: 400, error: "invalid data key" };
      }
      if (typeof value !== "string") {
        return { ok: false, status: 400, error: "invalid data value" };
      }
      data[key] = clampString(value, MAX_DATA_VALUE_LENGTH);
    }
  }

  return {
    ok: true,
    value: {
      token,
      title: clampString((raw.title as string | undefined) ?? "", MAX_TITLE_LENGTH),
      body: clampString((raw.body as string | undefined) ?? "", MAX_NOTIFICATION_BODY_LENGTH),
      data,
    },
  };
}

// ---- /register (public self-service) ---------------------------------------

/**
 * Whether public self-registration is open. Closed unless explicitly opened.
 *
 * Defaulting to OPEN meant every deployment of this Worker that had not read the
 * docs ran an unauthenticated key-minting endpoint on the public internet. The
 * failure is asymmetric: an operator who wants open registration flips one var
 * and finds out immediately when they try to register; one who did not want it
 * finds out when someone else is using their relay.
 */
export function registrationEnabled(env: CommonEnv): boolean {
  return (env.REGISTRATION_ENABLED ?? "").trim().toLowerCase() === "true";
}

/**
 * Public, unauthenticated self-registration: a self-hosted server obtains its
 * own per-server key with no maintainer involvement.
 *
 * One active key per IP: registering from an IP that already holds a key
 * supersedes the previous one, so a server that loses its key file can simply
 * re-register. Its old key then answers `key_superseded` (see denyKey), so a
 * holder still presenting it can re-register in turn rather than failing
 * forever. That bounds only *concurrent* keys per IP, so it is paired with a
 * per-IP minute-tier rate limit (REGISTER_RATE_LIMITER) and the
 * REGISTRATION_ENABLED kill-switch.
 *
 * KNOWN LIMIT, stated rather than implied: an IP is not an identity. Two
 * self-hosters behind one CGNAT address take each other's key away by
 * registering, and one actor with many addresses accumulates many permanent
 * keys. Neither is fixed here, because both need a durable identity this
 * endpoint does not have — an invite code, an operator approval step, or a
 * credential that survives an address change — and inventing one silently
 * inside the registration path would be worse than the gap.
 *
 * What IS bounded, as of RELAY_DAILY_BUDGET (see checkDailyBudget), is the thing
 * the accumulation was worth doing: the relay's total daily sends are now one
 * shared counter, so extra keys no longer buy extra budget. The remaining
 * exposure from key accumulation is denial-of-service against that shared pool,
 * rather than unbounded sending. Each registration also writes a few registry
 * rows, which the per-IP limiter bounds and no counter here can see.
 *
 * The CGNAT collision has no mitigation here at all. An operator running a relay
 * for users who may share an address should keep REGISTRATION_ENABLED off and
 * issue keys out of band.
 *
 * "One" is a single registry transaction: supersede every active key for the
 * IP, insert the new one. The key is minted before it and handed out only after
 * it commits, so no interleaving of registrations leaves two active keys, and a
 * failed transaction leaves nothing behind. (Under KV this took a claim, a mint
 * and a commit across three systems, and still had to be patched twice.)
 */
export async function handleRegister(request: Request, rc: RequestContext): Promise<Response> {
  const { env } = rc;
  if (!registrationEnabled(env)) {
    return fail(rc, 403, "self-registration is disabled");
  }
  // Bucket on the /64 for IPv6 so a single allocation can't defeat the per-IP
  // registration limit and one-key-per-IP rule by rotating source addresses.
  const registeredIp = ipBucket(request.headers.get("CF-Connecting-IP") ?? "");
  // No usable client address means no rate limit and no one-key-per-IP
  // invariant, and both of those are the entire containment around an
  // unauthenticated endpoint that mints permanent credentials. Same fail-closed
  // reasoning as checkMinuteLimit and claimTokenForSend.
  if (!registeredIp) {
    rc.log({ level: "error", event: "register.denied", reason: "no_client_ip" });
    return fail(rc, 503, "registration is temporarily unavailable");
  }
  if (!(await checkMinuteLimit(env.REGISTER_RATE_LIMITER, rc, registeredIp))) {
    rc.log({ level: "warn", event: "register.denied", reason: "rate_limited", ip: registeredIp });
    const response = fail(rc, 429, "too many registration attempts, try again later", {
      window: "minute",
      retryAfterSeconds: 60,
    });
    response.headers.set("Retry-After", "60");
    return response;
  }
  const parsed = await jsonObjectBody<{ label?: string }>(request);
  if (!parsed.ok) {
    return fail(rc, parsed.status, parsed.error);
  }
  // Clamped so no caller can store an unbounded label (see MAX_LABEL_LENGTH).
  const label =
    ((typeof parsed.value.label === "string" ? parsed.value.label : "").trim() || "self-registered").slice(
      0,
      MAX_LABEL_LENGTH,
    );

  // Without the registry the one-key-per-IP rule is unenforceable, and an
  // unconstrained permanent key on a public endpoint is what it guards.
  const registry = keyRegistry(env);
  if (!registry) {
    rc.log({ level: "error", event: "register.denied", reason: "coordinator_binding_missing", ip: registeredIp });
    return fail(rc, 503, "registration is temporarily unavailable");
  }

  // Only the hash is stored; the raw key travels once, in this response.
  const key = randomToken();
  const id = crypto.randomUUID();
  const createdAt = Date.now();
  const displaced = await registry.registerKey({ hash: await sha256Hex(key), id, label, ip: registeredIp, createdAt });

  rc.log({ level: "info", event: "key.created", keyId: id, label, source: "self" });
  for (const old of displaced) {
    keyCache.delete(old.hash);
    rc.log({ level: "info", event: "key.superseded", keyId: old.id, ip: registeredIp });
  }
  return json({ id, label, key, expiresAt: null }, 201);
}

// ---- router / fetch wrapper -------------------------------------------------
//
// The two workers' routing (health/send/register dispatch) and their
// `export default { fetch(...) }` wrapper (request-id minting, structured access
// logging, unhandled-error catch) are identical; only `/health`'s `configured`
// flag and `/send`'s delivery logic are provider-specific.
// createRelayFetchHandler takes those two pieces and returns each worker's
// `fetch`.

export interface RelayRouterOptions<TEnv extends CommonEnv> {
  /** Whether the provider (FCM/APNs) credentials are fully configured, surfaced on /health. */
  configured: (env: TEnv) => boolean;
  /** Provider-specific POST /send handler. */
  handleSend: (request: Request, rc: RequestContext<TEnv>) => Promise<Response>;
}

async function routeRelay<TEnv extends CommonEnv>(
  request: Request,
  path: string,
  rc: RequestContext<TEnv>,
  opts: RelayRouterOptions<TEnv>,
): Promise<Response> {
  const { env } = rc;

  if (path === "/health" && request.method === "GET") {
    return json({
      ok: true,
      configured: opts.configured(env),
      rateLimits: { perMinute: resolveLimit(env.RATE_LIMIT_PER_MINUTE, DEFAULT_LIMIT_PER_MINUTE) },
      registrationEnabled: registrationEnabled(env),
    });
  }

  if (path === "/send" && request.method === "POST") {
    return opts.handleSend(request, rc);
  }

  if (path === "/register" && request.method === "POST") {
    return handleRegister(request, rc);
  }

  // No admin surface, deliberately. There used to be `/admin/keys` (mint, list,
  // revoke) behind a bearer ADMIN_SECRET, and nothing called it: the Go server
  // uses /register and /send, the mobile apps never touch the relay, and the
  // endpoints existed only for the maintainer to curl. That made the highest
  // value credential in the deployment — one that mints and revokes every key
  // the relay honours — permanently guessable by anyone who found the hostname,
  // to save a `wrangler kv key delete`. Deleting the door beat guarding it. Key
  // management is now the CLI; see the READMEs.
  return fail(rc, 404, "not found");
}

/** Builds the `fetch` export for a relay worker from its provider-specific pieces. */
export function createRelayFetchHandler<TEnv extends CommonEnv>(opts: RelayRouterOptions<TEnv>) {
  return async function fetch(request: Request, env: TEnv, ctx: ExecutionContext): Promise<Response> {
    const requestId = crypto.randomUUID();
    const start = Date.now();
    const url = new URL(request.url);
    const path = url.pathname.replace(/\/+$/, "") || "/";
    const log = (fields: Record<string, unknown>) =>
      console.log(JSON.stringify({ ts: new Date().toISOString(), requestId, ...fields }));

    const rc: RequestContext<TEnv> = { env, ctx, requestId, log };

    let response: Response;
    try {
      response = await routeRelay(request, path, rc, opts);
    } catch (err) {
      log({ level: "error", event: "unhandled", method: request.method, path, error: errorMessage(err) });
      response = json({ error: "internal error", requestId }, 500);
    }

    response.headers.set("X-Request-Id", requestId);
    log({
      level: response.status >= 500 ? "error" : "info",
      event: "request",
      method: request.method,
      path,
      status: response.status,
      durationMs: Date.now() - start,
    });
    return response;
  };
}
