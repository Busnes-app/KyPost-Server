/**
 * A rejected APNs provider token must be evicted from the cache immediately.
 *
 * Apple rejects a provider (auth) token with HTTP 403 and a JSON body whose
 * reason is "ExpiredProviderToken" or "InvalidProviderToken" (e.g. an auth key
 * scoped to the wrong environment). The relay caches the signed token for 29
 * minutes, so if it does not evict on rejection, every send fails until the
 * cache expires. That is the production incident of 2026-10-22, where the only
 * recovery was deleting the cache key by hand.
 *
 * The old classifier matched the substrings "expiredtoken"/"invalidtoken",
 * which do NOT appear in Apple's "ExpiredProviderToken"/"InvalidProviderToken",
 * so the eviction branch never ran. These tests use Apple's real bodies.
 *
 *   node --test worker-apns/src/apns-provider-token.test.mts
 */
import { test } from "node:test";
import assert from "node:assert/strict";
import { registerHooks } from "node:module";

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier.startsWith(".") && !/\.[cm]?[jt]s$/.test(specifier)) {
      return nextResolve(specifier + ".ts", context);
    }
    return nextResolve(specifier, context);
  },
});

const { sendApnsMessage, apnsReason } = await import("./apns.ts");

// A cached provider token is returned by cache.get, so ES256 signing (and the
// auth key) is never exercised here.
const config = {
  authKey: "unused",
  keyId: "KEYID12345",
  teamId: "TEAMID1234",
  topic: "org.kysecurity.mail",
  environment: "production" as const,
};

function stubCache() {
  const deleted: string[] = [];
  const cache = {
    async get() {
      return "cached-provider-token";
    },
    async put() {},
    async delete(key: string) {
      deleted.push(key);
    },
  };
  return { cache, deleted };
}

async function sendAgainst(status: number, body: string) {
  const { cache, deleted } = stubCache();
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async () => new Response(body, { status })) as unknown as typeof fetch;
  try {
    const result = await sendApnsMessage(config, cache as unknown as KVNamespace, {
      token: "device-token",
      title: "t",
      body: "b",
    });
    return { result, deleted };
  } finally {
    globalThis.fetch = realFetch;
  }
}

test("apnsReason extracts Apple's reason and tolerates non-JSON", () => {
  assert.equal(apnsReason(JSON.stringify({ reason: "ExpiredProviderToken" })), "ExpiredProviderToken");
  assert.equal(apnsReason(JSON.stringify({ reason: "Unregistered", timestamp: 1710000000000 })), "Unregistered");
  assert.equal(apnsReason("<html>Bad Gateway</html>"), "");
  assert.equal(apnsReason(""), "");
});

for (const reason of ["ExpiredProviderToken", "InvalidProviderToken"]) {
  test("403 " + reason + " evicts the cached provider token and asks the backend to retry", async () => {
    const { result, deleted } = await sendAgainst(403, JSON.stringify({ reason }));
    assert.equal(result.ok, false);
    assert.deepEqual(deleted, ["apns_provider_token"], "the cached provider token was not evicted");
    if (result.ok === false) {
      assert.equal(result.status, 502, "the backend must be asked to retry");
      assert.equal(result.stale, false, "a provider-token fault must never delete the device");
    }
  });
}

test("an unrelated 403 leaves the cached provider token in place", async () => {
  const { result, deleted } = await sendAgainst(403, JSON.stringify({ reason: "Forbidden" }));
  assert.equal(result.ok, false);
  assert.deepEqual(deleted, [], "an unrelated 403 must not evict the provider token");
});
