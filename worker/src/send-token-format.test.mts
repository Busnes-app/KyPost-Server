/**
 * The FCM relay pins a device by sha256(token), so a second spelling of one
 * token is a second, unpinned claim. FCM carries the token in a JSON body, not
 * a URL path, but the pin still has to see exactly one spelling: anything else
 * is refused before the claim, the budget or Google.
 *
 *   node --test worker/src/send-token-format.test.mts
 */

import { test } from "node:test";
import assert from "node:assert/strict";
import { registerHooks } from "node:module";

const durableObjectStub =
  "data:text/javascript," +
  encodeURIComponent("export class DurableObject{constructor(ctx,env){this.ctx=ctx;this.env=env}}");

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier === "cloudflare:workers") {
      return { url: durableObjectStub, shortCircuit: true };
    }
    if (specifier.startsWith(".") && !/\.[cm]?[jt]s$/.test(specifier)) {
      return nextResolve(specifier + ".ts", context);
    }
    return nextResolve(specifier, context);
  },
});

const worker = await import("./index.ts");
const { RelayCoordinator } = await import("../../push-relay-shared/relay-coordinator.ts");
const { durableStorage } = await import("../../push-relay-shared/durable-storage.mts");
const { KEY_PREFIX, KEY_INDEX_PREFIX, BOUND_TOKEN_PREFIX, sha256Hex } = await import(
  "../../push-relay-shared/push-relay-common.ts"
);

const API_KEY = "relay-key-under-test";
/** A real-shaped FCM registration token, distinct per seed. */
const device = (seed: string) => `${seed}TxbVurMB3E:APA91b` + "x_-Y".repeat(35);

// Durable Object storage with a real SQLite behind `sql`, as the key registry needs.
const storage = durableStorage;

/** A configured FCM env with a budget of one send and `victim` owned by another key. */
async function env(victim: string) {
  const kv = new Map<string, unknown>([
    [KEY_PREFIX + (await sha256Hex(API_KEY)), { id: "key-under-test", label: "t", enabled: true, createdAt: "2026-01-01T00:00:00Z" }],
    [BOUND_TOKEN_PREFIX + (await sha256Hex(victim)), "key-someone-else"],
    [KEY_INDEX_PREFIX + "key-someone-else", "raw-other"],
    [KEY_PREFIX + "raw-other", { id: "key-someone-else", label: "o", enabled: true, createdAt: "2026-01-01T00:00:00Z" }],
  ]);
  const instances = new Map();
  return {
    instances,
    FCM_CLIENT_EMAIL: "relay@example.iam.gserviceaccount.com",
    FCM_PRIVATE_KEY: "present; never used, the access token is cached",
    FCM_PROJECT_ID: "kypost-test",
    OAUTH_CACHE: { async get() { return "cached-access-token"; }, async put() {}, async delete() {} },
    RELAY_DAILY_BUDGET: "1",
    RATELIMIT_FAIL_OPEN: "true",
    API_KEYS: {
      async get(key: string) {
        return kv.get(key) ?? null;
      },
    },
    RELAY_COORDINATOR: {
      idFromName: (name: string) => name,
      get(name: string) {
        if (!instances.has(name)) instances.set(name, new RelayCoordinator({ storage: storage() }, {}));
        return instances.get(name);
      },
    },
  };
}

const ctx = { waitUntil(p: Promise<unknown>) { void p; } };

function send(e: unknown, token: string) {
  return worker.default.fetch(
    new Request("https://relay.example/send", {
      method: "POST",
      headers: { Authorization: "Bearer " + API_KEY },
      body: JSON.stringify({ token, title: "t", body: "b" }),
    }),
    e,
    ctx,
  );
}

test("an aliased or malformed token is refused before the claim, the budget or Google", async () => {
  const victim = device("aa");
  const e = await env(victim);

  const realFetch = globalThis.fetch;
  const sent: string[] = [];
  globalThis.fetch = (async (_url: string, init: RequestInit) => {
    sent.push(JSON.parse(String(init.body)).message.token);
    return new Response("{}", { status: 200 });
  }) as typeof fetch;

  try {
    for (const alias of [
      `a/../${victim}`,
      `%2e%2e/${victim}`,
      `${victim}#x`,
      `${victim}?x`,
      `${victim}.`,
      `${victim.slice(0, 40)} ${victim.slice(40)}`,
      victim.repeat(4),
    ]) {
      const refused = await send(e, alias);
      assert.equal(refused.status, 400, `${JSON.stringify(alias)} answered ${refused.status}`);
      assert.equal((await refused.json()).error, "invalid token");
    }
    // The key registry is consulted to authenticate; nothing past it may be.
    assert.deepEqual([...e.instances.keys()].filter((n) => n !== "relay-keys"), [], "a refused token touched a coordinator or the budget");

    // Surrounding whitespace folds onto the victim's own pin rather than a fresh one.
    assert.equal((await send(e, ` ${victim}\n`)).status, 403);
    assert.equal(sent.length, 0, "a refused token reached Google");

    // Budget is 1: none of the refusals above may have spent it.
    const free = device("bb");
    assert.equal((await send(e, `\t${free} `)).status, 200);
    assert.deepEqual(sent, [free]);
  } finally {
    globalThis.fetch = realFetch;
  }
});
