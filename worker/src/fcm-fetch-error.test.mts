/**
 * A send whose fetch throws keeps its daily-budget draw.
 *
 * The throw cannot be told apart from a failure reading a response FCM did
 * send, so it is answered as reachedProvider: true and handleSend does not
 * refund it — the same rule worker-apns follows (push-relay-shared/AGENTS.md).
 *
 *   node --test worker/src/fcm-fetch-error.test.mts
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

const { sendFcmMessage } = await import("./fcm.ts");

/** A cache already holding an access token, so no OAuth exchange runs. */
const warmCache = {
  get: async () => "cached-access-token",
  put: async () => undefined,
  delete: async () => undefined,
} as unknown as KVNamespace;

const config = { clientEmail: "relay@example.iam.gserviceaccount.com", privateKeyPem: "", projectId: "p" };

test("a fetch that throws is answered, not thrown, and counts as reaching FCM", async () => {
  const realFetch = globalThis.fetch;
  let calls = 0;
  globalThis.fetch = (async () => {
    calls++;
    throw new TypeError("network connection lost");
  }) as typeof fetch;
  try {
    const result = await sendFcmMessage(config, warmCache, { token: "device-token", title: "t", body: "b" });
    assert.equal(calls, 1, "the send request was attempted");
    assert.equal(result.ok, false);
    assert.ok(!result.ok && result.reachedProvider, "unproven means not refunded");
    assert.ok(!result.ok && !result.stale, "a network error never retires a device");
    assert.ok(!result.ok && result.status === 502);
  } finally {
    globalThis.fetch = realFetch;
  }
});
