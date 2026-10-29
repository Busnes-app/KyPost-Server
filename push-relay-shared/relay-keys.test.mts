/**
 * Key records in the RelayCoordinator registry: registration, supersede, one
 * active key per IP, token takeover, the KV fallback with copy-on-use, unknown
 * versus revoked, and the migration cases where KV and the registry disagree.
 *
 * The registry's SQL runs against a real in-memory SQLite (durable-storage.mts).
 *
 *   node --test push-relay-shared/relay-keys.test.mts
 */

import { test, beforeEach } from "node:test";
import assert from "node:assert/strict";
import { registerHooks } from "node:module";

registerHooks({
  resolve(specifier, context, nextResolve) {
    return specifier === "cloudflare:workers"
      ? {
          url:
            "data:text/javascript," +
            encodeURIComponent("export class DurableObject{constructor(ctx,env){this.ctx=ctx;this.env=env}}"),
          shortCircuit: true,
        }
      : nextResolve(specifier, context);
  },
});

const { RelayCoordinator } = await import("./relay-coordinator.ts");
const { durableStorage } = await import("./durable-storage.mts");
const {
  handleRegister,
  lookupKey,
  denyKey,
  claimTokenForSend,
  settleToken,
  clearKeyCache,
  keyRegistry,
  sha256Hex,
  KEY_REGISTRY,
  KEY_PREFIX,
  KEY_INDEX_PREFIX,
  SUPERSEDED_PREFIX,
} = await import("./push-relay-common.ts");

beforeEach(() => clearKeyCache());

// ---- doubles ---------------------------------------------------------------

const allow = { async limit() { return { success: true }; } };

/** A relay env whose KV is read-only: any write to it fails the test. */
function relayEnv({ kv = new Map(), limiter = allow, coordinator = true } = {}) {
  const instances = new Map();
  const kvReads = [];
  const refuse = async (key) => {
    throw new Error("wrote KV: " + key);
  };
  return {
    instances,
    kvReads,
    REGISTRATION_ENABLED: "true",
    REGISTER_RATE_LIMITER: limiter,
    API_KEYS: {
      async get(key) {
        kvReads.push(key);
        const value = kv.get(key);
        return value === undefined ? null : value;
      },
      put: refuse,
      delete: refuse,
    },
    RELAY_COORDINATOR: coordinator
      ? {
          idFromName: (name) => name,
          get(name) {
            if (!instances.has(name)) {
              instances.set(name, new RelayCoordinator({ storage: durableStorage() }, {}));
            }
            return instances.get(name);
          },
        }
      : undefined,
  };
}

function rc(env, logs = []) {
  return { env, ctx: { waitUntil() {} }, requestId: "test", log: (fields) => logs.push(fields) };
}

async function register(env, ip = "203.0.113.7", logs = []) {
  const response = await handleRegister(
    new Request("https://relay.example/register", {
      method: "POST",
      body: JSON.stringify({ label: "server" }),
      headers: { "CF-Connecting-IP": ip },
    }),
    rc(env, logs),
  );
  return { status: response.status, body: response.status === 201 ? await response.json() : null };
}

async function statusOf(env, key) {
  return (await lookupKey(rc(env), await sha256Hex(key))).status;
}

/** What /send answers for a key: status plus the `code` a server acts on. */
async function sendAnswer(env, key) {
  const found = await lookupKey(rc(env), await sha256Hex(key));
  if (found.status === "active") {
    return { status: 200 };
  }
  const response = denyKey(rc(env), found);
  return { status: response.status, code: (await response.json()).code };
}

const sql = (env, query, ...bindings) => keyRegistry(env).ctx.storage.sql.exec(query, ...bindings).toArray();
const activeIds = (env, ip) => sql(env, "SELECT id FROM keys WHERE ip = ? AND status = 'active'", ip).map((r) => r.id);

/** KV contents for one pre-registry key, as /register used to write them. */
async function kvKey(rawKey, record) {
  const hash = await sha256Hex(rawKey);
  return [
    [KEY_PREFIX + hash, { label: record.id, enabled: true, createdAt: "2026-10-01T00:00:00Z", source: "self", ...record }],
    [KEY_INDEX_PREFIX + record.id, hash],
  ];
}

// ---- registration ----------------------------------------------------------

test("registration stores only the key's hash in the registry and writes no KV", async () => {
  const env = relayEnv();
  const { status, body } = await register(env);
  assert.equal(status, 201);
  assert.match(body.key, /^[0-9a-f]{64}$/);

  const [row] = sql(env, "SELECT * FROM keys");
  assert.equal(row.hash, await sha256Hex(body.key));
  assert.equal(row.id, body.id);
  assert.equal(row.ip, "203.0.113.7");
  assert.equal(row.status, "active");
  assert.equal(row.from_kv, 0);
  assert.ok(!JSON.stringify(row).includes(body.key), "the raw key was stored");
  assert.equal(await statusOf(env, body.key), "active");
  assert.deepEqual([...env.instances.keys()], [KEY_REGISTRY], "registration touched more than the registry");
});

test("re-registering from an IP supersedes its key, which then answers key_superseded", async () => {
  const env = relayEnv();
  const first = (await register(env)).body;
  const second = (await register(env)).body;

  assert.deepEqual(await sendAnswer(env, first.key), { status: 401, code: "key_superseded" });
  assert.deepEqual(await sendAnswer(env, second.key), { status: 200 });
  assert.deepEqual(activeIds(env, "203.0.113.7"), [second.id]);
});

test("concurrent registrations from one IP leave exactly one active key", async () => {
  const env = relayEnv();
  const results = await Promise.all(Array.from({ length: 8 }, () => register(env)));
  assert.ok(results.every((r) => r.status === 201));
  const active = activeIds(env, "203.0.113.7");
  assert.equal(active.length, 1, `${active.length} active keys for one address`);
  const live = [];
  for (const { body } of results) {
    if ((await statusOf(env, body.key)) === "active") live.push(body.id);
  }
  assert.deepEqual(live, active);
});

test("one IPv6 /64 is one slot, and another address's slot is untouched", async () => {
  const env = relayEnv();
  const other = (await register(env, "198.51.100.9")).body;
  await register(env, "2001:db8:1:2:3:4:5:6");
  const second = (await register(env, "2001:db8:1:2:aaaa:bbbb:cccc:dddd")).body;

  assert.deepEqual(activeIds(env, "2001:db8:1:2::/64"), [second.id]);
  assert.equal(await statusOf(env, other.key), "active", "a registration elsewhere took another server's slot");
});

test("a registration that fails to commit changes nothing", async () => {
  const env = relayEnv();
  const first = (await register(env)).body;
  const registry = keyRegistry(env);
  // Same id as the live key: the INSERT violates UNIQUE(id) after the UPDATE
  // has already superseded the live key, so only a rollback keeps it.
  assert.throws(() =>
    registry.registerKey({ hash: "f".repeat(64), id: first.id, label: "x", ip: "203.0.113.7", createdAt: Date.now() }),
  );
  assert.equal(await statusOf(env, first.key), "active", "a failed registration superseded the live key");
});

// ---- the controls that existed before still hold ----------------------------

test("registration fails closed without its limiter, its registry or a client address", async () => {
  const limited = relayEnv({ limiter: { async limit() { return { success: false }; } } });
  assert.equal((await register(limited)).status, 429);

  const noLimiter = relayEnv({ limiter: null });
  assert.equal((await register(noLimiter)).status, 429);

  const noRegistry = relayEnv({ coordinator: false });
  assert.equal((await register(noRegistry)).status, 503);
  assert.equal((await register(relayEnv(), "unknown")).status, 503);

  for (const env of [limited, noLimiter]) {
    assert.equal(env.instances.size, 0, "a refused registration reached the registry");
  }
});

test("a send fails closed when the registry binding is missing", async () => {
  const env = relayEnv({ coordinator: false });
  const found = await lookupKey(rc(env), await sha256Hex("anything"));
  assert.equal(found.status, "unavailable");
  assert.equal(denyKey(rc(env), found).status, 503);
});

test("no key material reaches the logs", async () => {
  const kv = new Map(await kvKey("legacy-raw-key", { id: "legacy" }));
  const env = relayEnv({ kv });
  const logs = [];
  const { body } = await register(env, "203.0.113.7", logs);
  await register(env, "203.0.113.7", logs);
  for (const key of [body.key, "legacy-raw-key", "never-issued"]) {
    const found = await lookupKey(rc(env, logs), await sha256Hex(key));
    if (found.status !== "active") denyKey(rc(env, logs), found);
  }
  const text = JSON.stringify(logs);
  assert.ok(logs.length > 0);
  for (const secret of [body.key, await sha256Hex(body.key), "legacy-raw-key", await sha256Hex("legacy-raw-key")]) {
    assert.ok(!text.includes(secret), "a key or its hash was logged");
  }
});

// ---- unknown versus revoked (#165) -------------------------------------------

test("a key nobody knows answers key_unknown; a revoked one stays a plain refusal", async () => {
  const env = relayEnv();
  assert.deepEqual(await sendAnswer(env, "never-issued"), { status: 401, code: "key_unknown" });

  const { body } = await register(env);
  sql(env, "UPDATE keys SET status = 'revoked' WHERE id = ?", body.id); // what an operator runs in Data Studio
  assert.deepEqual(await sendAnswer(env, body.key), { status: 401, code: undefined });
});

test("KV's own refusals keep their meaning after the move", async () => {
  const kv = new Map([
    ...(await kvKey("disabled-key", { id: "disabled", enabled: false })),
    [SUPERSEDED_PREFIX + (await sha256Hex("replaced-key")), "1"],
    ...(await kvKey("expired-key", { id: "expired", source: "admin", expiresAt: "2026-01-01T00:00:00Z" })),
  ]);
  const env = relayEnv({ kv });
  assert.deepEqual(await sendAnswer(env, "disabled-key"), { status: 401, code: undefined });
  assert.deepEqual(await sendAnswer(env, "replaced-key"), { status: 401, code: "key_superseded" });
  assert.equal((await sendAnswer(env, "expired-key")).status, 401);
  assert.equal(await statusOf(env, "expired-key"), "expired");
});

// ---- KV fallback and copy-on-use ---------------------------------------------

test("a key only in KV works, is copied in on first use, and KV is not asked again", async () => {
  const kv = new Map(await kvKey("legacy-key", { id: "legacy", registeredIp: "203.0.113.7" }));
  const env = relayEnv({ kv });

  assert.equal(await statusOf(env, "legacy-key"), "active");
  const [row] = sql(env, "SELECT id, ip, status, from_kv FROM keys");
  assert.deepEqual(row, { id: "legacy", ip: "203.0.113.7", status: "active", from_kv: 1 });

  kv.clear(); // KV loses it: the registry copy is now the record
  env.kvReads.length = 0;
  clearKeyCache();
  assert.equal(await statusOf(env, "legacy-key"), "active");
  assert.deepEqual(env.kvReads, [], "a copied key was looked up in KV again");
});

test("a copied-in legacy key is superseded by the next registration from its IP", async () => {
  const kv = new Map(await kvKey("legacy-key", { id: "legacy", registeredIp: "203.0.113.7" }));
  const env = relayEnv({ kv });
  assert.equal(await statusOf(env, "legacy-key"), "active");

  await register(env);
  clearKeyCache();
  assert.deepEqual(await sendAnswer(env, "legacy-key"), { status: 401, code: "key_superseded" });
});

// ---- KV and the registry disagree ---------------------------------------------

test("when KV and the registry disagree, the registry wins", async () => {
  const kv = new Map();
  const env = relayEnv({ kv });
  const superseded = (await register(env)).body;
  const revoked = (await register(env, "198.51.100.9")).body;
  const live = (await register(env)).body; // supersedes `superseded`
  sql(env, "UPDATE keys SET status = 'revoked' WHERE id = ?", revoked.id);

  // KV still holds every one of them as enabled, as a stale or restored
  // namespace would.
  for (const k of [superseded, revoked]) {
    for (const [cell, value] of await kvKey(k.key, { id: k.id })) kv.set(cell, value);
  }
  clearKeyCache();
  assert.deepEqual(await sendAnswer(env, superseded.key), { status: 401, code: "key_superseded" });
  assert.deepEqual(await sendAnswer(env, revoked.key), { status: 401, code: undefined });
  assert.equal(await statusOf(env, live.key), "active", "a key missing from KV was refused");
});

test("a KV key with a different record under the same id cannot displace a registry key", async () => {
  const env = relayEnv();
  const { body } = await register(env);
  // A KV record claiming the registry key's id, under another hash.
  const kv = new Map(await kvKey("impostor", { id: body.id, registeredIp: "198.51.100.9" }));
  const both = relayEnv({ kv });
  both.RELAY_COORDINATOR = env.RELAY_COORDINATOR;
  assert.notEqual(await statusOf(both, "impostor"), "active");
  assert.equal(await statusOf(both, body.key), "active");
  assert.deepEqual(sql(env, "SELECT id, ip FROM keys"), [{ id: body.id, ip: "203.0.113.7" }]);
});

test("a KV key its IP has re-registered since is copied in superseded, not active", async () => {
  // The lost-key server: its old key is still in KV, never used since the
  // registry went live, and it has registered a new one.
  const kv = new Map(await kvKey("old-key", { id: "old", registeredIp: "203.0.113.7" }));
  const env = relayEnv({ kv });
  const fresh = (await register(env)).body;

  assert.deepEqual(await sendAnswer(env, "old-key"), { status: 401, code: "key_superseded" });
  assert.equal(await statusOf(env, fresh.key), "active");
  assert.deepEqual(activeIds(env, "203.0.113.7"), [fresh.id]);
});

// ---- token takeover ---------------------------------------------------------

async function ageClaim(env, token) {
  const instance = env.instances.get("token:" + (await sha256Hex(token)));
  await instance.ctx.storage.put("claimedAt", Date.now() - 120_000);
}

test("a token follows a server to its new key, and never to another server", async () => {
  const env = relayEnv();
  const a1 = (await register(env, "203.0.113.7")).body;
  const b = (await register(env, "198.51.100.9")).body;

  assert.deepEqual(await claimTokenForSend(rc(env), "device-token", a1.id), { allowed: true });
  await settleToken(rc(env), "device-token", a1.id, true);
  await ageClaim(env, "device-token");

  const stolen = await claimTokenForSend(rc(env), "device-token", b.id);
  assert.equal(stolen.allowed, false, "another server took over a live key's token");

  const a2 = (await register(env, "203.0.113.7")).body; // a1's server re-registers
  assert.deepEqual(await claimTokenForSend(rc(env), "device-token", a2.id), { allowed: true });
  const late = await claimTokenForSend(rc(env), "device-token", b.id);
  assert.equal(late.allowed, false);
});

test("a token pinned to a KV-only key moves to that server's new key", async () => {
  // Migration: the pin and the key are both pre-registry, the key was never
  // used since, and its server has re-registered.
  const kv = new Map(await kvKey("old-key", { id: "old", registeredIp: "203.0.113.7" }));
  const env = relayEnv({ kv });
  assert.deepEqual(await claimTokenForSend(rc(env), "device-token", "old"), { allowed: true });
  await settleToken(rc(env), "device-token", "old", true);
  await ageClaim(env, "device-token");

  const outsider = (await register(env, "198.51.100.9")).body;
  assert.equal((await claimTokenForSend(rc(env), "device-token", outsider.id)).allowed, false);

  const fresh = (await register(env, "203.0.113.7")).body;
  assert.deepEqual(await claimTokenForSend(rc(env), "device-token", fresh.id), { allowed: true });
});

// ---- the in-Worker cache ------------------------------------------------------

test("the cache serves active keys briefly and forgets keys superseded here", async () => {
  const env = relayEnv();
  const first = (await register(env)).body;
  const hash = await sha256Hex(first.key);
  const now = Date.now();
  assert.equal((await lookupKey(rc(env), hash, now)).status, "active");

  // Revoked out of band: this isolate keeps serving it until the entry ages out.
  sql(env, "UPDATE keys SET status = 'revoked' WHERE id = ?", first.id);
  assert.equal((await lookupKey(rc(env), hash, now + 1_000)).status, "active");
  assert.equal((await lookupKey(rc(env), hash, now + 31_000)).status, "revoked");

  // Superseded by a registration this isolate handled: forgotten at once.
  const second = (await register(env)).body;
  const secondHash = await sha256Hex(second.key);
  assert.equal((await lookupKey(rc(env), secondHash)).status, "active");
  await register(env);
  assert.equal((await lookupKey(rc(env), secondHash)).status, "superseded");
});
