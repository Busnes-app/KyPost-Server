/**
 * RelayCoordinator — the strongly-consistent half of the relay, shared by both
 * relay Workers (FCM and APNs).
 *
 * Every instance routes by name, and one instance handles its requests one at a
 * time, so a read and the write that depends on it cannot be interleaved by
 * anyone else. KV cannot offer that: two concurrent requests both read "no owner
 * yet" and both write themselves in. Three kinds of instance:
 *
 *   1. `token:<sha256(device token)>` — device-token ownership ("the first key to
 *      deliver to a token owns it"). Without serialization, two keys racing the
 *      first send to one token were both let through: the spoofed-notification
 *      path the pinning exists to close.
 *
 *   2. KEY_REGISTRY — every key record, in a SQLite `keys` table. Registration
 *      and the supersede it implies are one transaction, and one active key per
 *      IP is a query on the `ip` column. Records used to live in KV, where a
 *      registration cost four writes against the free plan's 1,000 a day; on
 *      2026-10-26 test servers registering spent that quota and Android push
 *      stopped. See the key-records section of push-relay-common.ts.
 *
 *   3. `relay-daily-budget` — the relay-wide daily send counter.
 *
 * The token instances stay nearly dumb: an owner id plus the bookkeeping that
 * makes releasing a claim safe (see settleToken). Whether an owning key is still
 * active is the registry's answer, so the caller asks it and comes back with a
 * compare-and-swap (`takeoverFrom`) rather than one instance calling another.
 */

import { DurableObject } from "cloudflare:workers";

/** Storage keys inside one token instance. The instance IS the token, so these are fixed. */
const OWNER_KEY = "owner";
const SEEDED_KEY = "seeded";
/** A send under the CURRENT claim actually delivered — see settleToken. */
const CONFIRMED_KEY = "confirmed";
/** Sends currently holding the current claim, so a rollback can't outrun one. */
const INFLIGHT_KEY = "inflight";
/** When the current claim was taken (ms epoch), for the caller's takeover guard. */
const CLAIMED_AT_KEY = "claimedAt";

export type KeyStatus = "active" | "superseded" | "revoked";

/** One row of the registry's `keys` table, as findKey returns it. */
export type KeyRow = {
  hash: string;
  id: string;
  label: string;
  ip: string | null;
  createdAt: number;
  /** Only on keys from the admin API that used to exist; self-registered keys never expire. */
  expiresAt: number | null;
  source: string;
  status: KeyStatus;
};

const KEY_COLUMNS = "hash, id, label, ip, created_at AS createdAt, expires_at AS expiresAt, source, status";

export interface NewKey {
  hash: string;
  id: string;
  label: string;
  ip: string;
  createdAt: number;
}

/** A key record read from the pre-registry KV namespace. */
export interface LegacyKey {
  hash: string;
  id: string;
  label: string;
  ip: string | null;
  createdAt: number;
  expiresAt: number | null;
  source: string;
  enabled: boolean;
}
/** UTC day (YYYY-MM-DD) the relay-wide budget count below belongs to. */
const BUDGET_DAY_KEY = "budgetDay";
/** Sends drawn from the relay-wide budget during BUDGET_DAY_KEY. */
const BUDGET_USED_KEY = "budgetUsed";

export interface ClaimTokenOptions {
  /** The key attempting to claim. */
  keyId: string;
  /**
   * Owner recorded in the pre-Durable-Object KV index, used ONCE to seed this
   * instance so tokens claimed before this change keep their owner. Ignored
   * afterwards — otherwise a token released as dead would silently re-adopt the
   * stale KV value on its next send.
   */
  legacyOwner?: string | null;
  /**
   * Compare-and-swap: take ownership only if the current owner is still this
   * value. The caller sets it after confirming that owner's key is revoked,
   * disabled, expired or deleted. Without the comparison, that confirmation is
   * itself a check-then-write and races a legitimate re-claim.
   */
  takeoverFrom?: string | null;
}

export interface ClaimTokenResult {
  /** Who owns the token after this call. Equal to keyId when the claim succeeded. */
  owner: string;
  /**
   * When `owner` took the claim (ms epoch). The caller refuses to take over a
   * claim younger than KV's convergence window, because the "is the owner still
   * active?" answer can still come from KV for a key not yet copied into the
   * registry — see claimTokenForSend. A claim whose age is unknown (adopted from the legacy KV
   * index, or written before this field existed) is stamped on first sight and
   * so counts as fresh once — see RelayCoordinator.claimedAt.
   */
  claimedAt: number;
}

export class RelayCoordinator extends DurableObject {
  /** Reads the owner, seeding once from the legacy KV index if this instance is new. */
  private async currentOwner(legacyOwner?: string | null): Promise<string | undefined> {
    const seeded = await this.ctx.storage.get<boolean>(SEEDED_KEY);
    if (seeded) {
      return this.ctx.storage.get<string>(OWNER_KEY);
    }
    const owner = (legacyOwner ?? "").trim() || undefined;
    // A seeded owner is CONFIRMED on arrival: it only exists because that key
    // already delivered under the pre-Durable-Object KV scheme, and an
    // unconfirmed claim is one that settleToken deletes after a single failed
    // send. It is stamped as claimed NOW for the reason in claimedAt() below.
    await this.ctx.storage.put(
      owner
        ? { [SEEDED_KEY]: true, [OWNER_KEY]: owner, [CONFIRMED_KEY]: true, [CLAIMED_AT_KEY]: Date.now() }
        : { [SEEDED_KEY]: true },
    );
    return owner;
  }

  /**
   * When the current claim was taken, stamping one that has no timestamp.
   *
   * A claim without one was either adopted from the legacy KV index or written
   * by an earlier version of this class, and in both cases its real age is
   * simply unknown — the KV index carries no timestamp, and the earlier schema
   * stored none. Reading that as 0 ("ancient") is a guess in the unsafe
   * direction: the caller's takeover guard exists because a key minted seconds
   * ago reads as ABSENT in KV, and an unknown-age claim can be seconds old
   * across exactly the deploy that introduces this bookkeeping. Unknown is
   * therefore treated as fresh, which costs at most one convergence window of
   * delay on a legitimate takeover, once per token.
   *
   * The stamp is PERSISTED rather than computed per call. Returning a new
   * Date.now() each time would keep every legacy claim permanently inside its
   * own grace window and make it un-takeover-able forever.
   */
  private async claimedAt(): Promise<number> {
    const stored = await this.ctx.storage.get<number>(CLAIMED_AT_KEY);
    if (typeof stored === "number") {
      return stored;
    }
    const now = Date.now();
    await this.ctx.storage.put(CLAIMED_AT_KEY, now);
    return now;
  }

  /** Takes the claim for keyId, resetting the per-claim bookkeeping. */
  private async takeClaim(keyId: string): Promise<ClaimTokenResult> {
    const claimedAt = Date.now();
    // inflight resets to 1 (this send) rather than incrementing: any send still
    // running against the displaced owner's claim can no longer touch this one,
    // because settleToken acts only for the key that currently owns the token.
    await this.ctx.storage.put({
      [OWNER_KEY]: keyId,
      [CLAIMED_AT_KEY]: claimedAt,
      [CONFIRMED_KEY]: false,
      [INFLIGHT_KEY]: 1,
    });
    return { owner: keyId, claimedAt };
  }

  /**
   * Claims a device token for keyId. Returns the owner after the call, which the
   * caller compares against keyId to decide allow/deny — the decision and the
   * write happen in one turn here, which is the whole point.
   *
   * Every allowed send — the one that creates the claim and every later one that
   * rides it — is counted in, and MUST be counted back out with settleToken.
   */
  async claimToken(opts: ClaimTokenOptions): Promise<ClaimTokenResult> {
    const owner = await this.currentOwner(opts.legacyOwner);
    if (owner === undefined) {
      return this.takeClaim(opts.keyId);
    }
    if (owner === opts.keyId) {
      const inflight = (await this.ctx.storage.get<number>(INFLIGHT_KEY)) ?? 0;
      await this.ctx.storage.put(INFLIGHT_KEY, inflight + 1);
      return { owner, claimedAt: await this.claimedAt() };
    }
    if (opts.takeoverFrom && owner === opts.takeoverFrom) {
      return this.takeClaim(opts.keyId);
    }
    return { owner, claimedAt: await this.claimedAt() };
  }

  /**
   * Ends one send's use of the claim, and releases the claim when it turns out
   * nothing was ever delivered under it. Returns true when the claim was
   * released.
   *
   * This is the rollback, and `delivered` plus the in-flight count are what make
   * it safe. The previous version released on the caller's word alone ("I created
   * this claim and my send failed, delete it"), which identified the claim only
   * by its owning key id — so two concurrent sends from one key, one failing and
   * one succeeding, could have the failing one delete ownership that the
   * successful one had just legitimately established, leaving the token free for
   * a different key to claim and spoof pushes at that device.
   *
   * Here the claim survives if ANY send under it delivered (`confirmed`, sticky
   * for the life of the claim) or if another send is still using it
   * (`inflight`) — regardless of which of the two calls the runtime happens to
   * schedule first. The bias is deliberate: a retained claim pins a token to a
   * key that owns it anyway, while a wrongly released one is the spoofing gap.
   *
   * ponytail: a send that never settles (worker eviction mid-request) leaks its
   * in-flight count and pins the token to that key for the life of the key. That
   * is the safe direction, and revoking or expiring the key frees the token
   * through the normal takeover path; an alarm-based sweep is the upgrade if
   * that ever proves too sticky.
   */
  async settleToken(keyId: string, delivered: boolean): Promise<boolean> {
    const owner = await this.ctx.storage.get<string>(OWNER_KEY);
    if (owner !== keyId) {
      return false; // displaced by a takeover, or already released
    }
    const inflight = Math.max(0, ((await this.ctx.storage.get<number>(INFLIGHT_KEY)) ?? 1) - 1);
    if (delivered) {
      await this.ctx.storage.put({ [CONFIRMED_KEY]: true, [INFLIGHT_KEY]: inflight });
      return false;
    }
    // Absent means this instance was written by the version of this class that
    // had no confirmed flag, and its owner got there the only way that version
    // allowed: by delivering. Defaulting to false would have let the first
    // failed send after a deploy unpin every claim made before it — the exact
    // spoofing gap this method exists to close. takeClaim always writes an
    // explicit false, so absent can only mean "predates this code".
    const confirmed = (await this.ctx.storage.get<boolean>(CONFIRMED_KEY)) ?? true;
    if (confirmed || inflight > 0) {
      await this.ctx.storage.put(INFLIGHT_KEY, inflight);
      return false;
    }
    // SEEDED_KEY deliberately stays: re-reading the legacy KV owner would let a
    // token released as dead silently re-adopt the stale value.
    await this.ctx.storage.delete([OWNER_KEY, CONFIRMED_KEY, INFLIGHT_KEY, CLAIMED_AT_KEY]);
    return true;
  }

  // ---- key registry (the KEY_REGISTRY instance only) -------------------------
  //
  // Statuses: `active`; `superseded`, by a later /register from the same IP;
  // `revoked`, by an operator (an UPDATE in the dashboard's Data Studio — there
  // is no admin API) or a KV record that was already disabled. A key with no
  // row and no KV record is unknown: /send says `key_unknown`, so a server whose
  // key the relay lost re-registers, while a revoked key stays refused.

  private keysReady = false;

  private keysTable(): SqlStorage {
    const sql = this.ctx.storage.sql;
    if (!this.keysReady) {
      sql.exec(
        `CREATE TABLE IF NOT EXISTS keys (
          hash       TEXT PRIMARY KEY,
          id         TEXT NOT NULL UNIQUE,
          label      TEXT NOT NULL,
          ip         TEXT,
          created_at INTEGER NOT NULL,
          expires_at INTEGER,
          source     TEXT NOT NULL,
          from_kv    INTEGER NOT NULL DEFAULT 0,
          status     TEXT NOT NULL CHECK (status IN ('active', 'superseded', 'revoked')),
          ended_at   INTEGER
        )`,
      );
      sql.exec("CREATE INDEX IF NOT EXISTS keys_by_ip ON keys (ip, status)");
      this.keysReady = true;
    }
    return sql;
  }

  /** The row for one key, by hash (a presented key) or id (a token's owner). */
  findKey(by: { hash: string } | { id: string }): KeyRow | null {
    const sql = this.keysTable();
    const [column, value] = "hash" in by ? ["hash", by.hash] : ["id", by.id];
    const [row] = sql.exec<KeyRow>(`SELECT ${KEY_COLUMNS} FROM keys WHERE ${column} = ?`, value).toArray();
    return row ?? null;
  }

  /**
   * Records a freshly minted key as the one active key for its IP, superseding
   * every other, in one transaction. Returns the keys it displaced.
   */
  registerKey(key: NewKey): { id: string; hash: string }[] {
    const sql = this.keysTable();
    return this.ctx.storage.transactionSync(() => {
      const displaced = sql
        .exec<{ id: string; hash: string }>("SELECT id, hash FROM keys WHERE ip = ? AND status = 'active'", key.ip)
        .toArray();
      sql.exec(
        "UPDATE keys SET status = 'superseded', ended_at = ? WHERE ip = ? AND status = 'active'",
        key.createdAt,
        key.ip,
      );
      sql.exec(
        "INSERT INTO keys (hash, id, label, ip, created_at, source, status) VALUES (?, ?, ?, ?, ?, 'self', 'active')",
        key.hash,
        key.id,
        key.label,
        key.ip,
        key.createdAt,
      );
      return displaced;
    });
  }

  /**
   * Copies a key record found in KV into the table, the first time it is used,
   * and returns the row now on file. An existing row always wins: once a key is
   * here, KV no longer has a say in it.
   *
   * A KV key is imported as superseded when its IP has registered here since it
   * was created. Under KV, that registration would have revoked it; without this
   * rule a lost-key server's old key, still in KV, would come back to life next
   * to its replacement, and a token pinned to it could not be taken over.
   */
  importKey(key: LegacyKey): KeyRow | null {
    const sql = this.keysTable();
    return this.ctx.storage.transactionSync(() => {
      const existing = this.findKey({ hash: key.hash });
      if (existing) {
        return existing;
      }
      let status: KeyStatus = key.enabled ? "active" : "revoked";
      if (
        status === "active" &&
        key.ip &&
        sql
          .exec("SELECT 1 FROM keys WHERE ip = ? AND from_kv = 0 AND created_at > ? LIMIT 1", key.ip, key.createdAt)
          .toArray().length > 0
      ) {
        status = "superseded";
      }
      sql.exec(
        `INSERT OR IGNORE INTO keys (hash, id, label, ip, created_at, expires_at, source, from_kv, status)
         VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`,
        key.hash,
        key.id,
        key.label,
        key.ip,
        key.createdAt,
        key.expiresAt,
        key.source,
        status,
      );
      return this.findKey({ hash: key.hash });
    });
  }


  /**
   * Draw one unit from the relay-wide daily budget, returning whether it was
   * available. Called on ONE well-known instance, so the count is aggregate
   * across every key and every IP.
   *
   * Aggregate is the point. The per-minute limiters bucket per key and per
   * registering IP, which bounds burst rate but not daily volume — and with
   * public registration open, a caller who wants more buckets simply registers
   * more keys. The resource actually being spent (the operator's FCM/APNs
   * quota) is one shared pool, so the ceiling on it has to be one shared
   * counter, or it is not a ceiling.
   *
   * The counter resets on the UTC day boundary rather than on a rolling window:
   * a rolling window needs the timestamps of every send in it, and this needs
   * two integers. The cost is that a day's budget can be spent in its first
   * minute; the minute limiter is what bounds that shape, and the two together
   * are what the note in push-relay-common.ts asks for.
   *
   * An over-budget call does NOT increment. A rejected caller retrying in a
   * loop would otherwise push the count arbitrarily far past the limit, which
   * changes nothing while the day lasts but makes the counter useless as a
   * report of what was actually spent.
   *
   * The get-then-put below needs no transaction() or blockConcurrencyWhile().
   * It reads like a lost-update race and is reported as one, so: input gates
   * make it safe. While a storage operation is outstanding the runtime delivers
   * no new events to the object, so two concurrent sends cannot both observe
   * the same `used`. Cloudflare's own "Rules of Durable Objects" gives exactly
   * this shape — `const value = await storage.get("count"); await
   * storage.put("count", value + 1)` — as the canonical example of code that is
   * safe because of gating.
   *
   * What breaks that guarantee is non-storage I/O: an `await fetch(...)` between
   * the read and the write opens the gate and lets another request interleave.
   * There is deliberately none here, and adding one would silently reintroduce
   * the race this comment says does not exist. Keep this method storage-only.
   */
  async spendDailyBudget(limit: number, now: number): Promise<{ allowed: boolean; used: number; day: string }> {
    const day = new Date(now).toISOString().slice(0, 10);
    const storedDay = await this.ctx.storage.get<string>(BUDGET_DAY_KEY);
    const used = storedDay === day ? (await this.ctx.storage.get<number>(BUDGET_USED_KEY)) ?? 0 : 0;

    if (used >= limit) {
      // Persist the day rollover even on a refusal, so the first call of a new
      // day that happens to be over an exhausted previous day's count still
      // lands on a fresh counter.
      if (storedDay !== day) {
        await this.ctx.storage.put({ [BUDGET_DAY_KEY]: day, [BUDGET_USED_KEY]: 0 });
      }
      return { allowed: false, used, day };
    }

    const next = used + 1;
    await this.ctx.storage.put({ [BUDGET_DAY_KEY]: day, [BUDGET_USED_KEY]: next });
    return { allowed: true, used: next, day };
  }

  /**
   * Give a draw back, for a send that provably never reached the provider.
   *
   * The budget measures what the relay spends against the operator's FCM/APNs
   * quota, so a request that failed before any provider request was made spent
   * none of it. Without this, one wrong credential burns the whole day on
   * traffic that never left Cloudflare, and the relay stays refused until UTC
   * midnight after the credential is fixed.
   *
   * Only for failures the caller can prove made no provider request — NOT for a
   * token the provider rejected. That request spent the quota, and refunding it
   * would make invented device tokens free and the ceiling meaningless.
   *
   * A refund that arrives after the UTC day rolls over is dropped rather than
   * taken off the new day's count: it belongs to a day that is already closed.
   * Storage-only for the same gating reason as spendDailyBudget — keep it so.
   */
  async refundDailyBudget(now: number): Promise<void> {
    const day = new Date(now).toISOString().slice(0, 10);
    if ((await this.ctx.storage.get<string>(BUDGET_DAY_KEY)) !== day) {
      return;
    }
    const used = (await this.ctx.storage.get<number>(BUDGET_USED_KEY)) ?? 0;
    await this.ctx.storage.put(BUDGET_USED_KEY, Math.max(0, used - 1));
  }
}
