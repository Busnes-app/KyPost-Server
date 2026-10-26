/**
 * A KV write the account quota refuses must not fail a send, nor make every send mint a token.
 *
 *   node --test worker/src/fcm-token-cache.test.mts
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

async function testPrivateKeyPem(): Promise<string> {
  const pair = (await crypto.subtle.generateKey(
    { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
    true,
    ["sign", "verify"],
  )) as CryptoKeyPair;
  const pkcs8 = await crypto.subtle.exportKey("pkcs8", pair.privateKey);
  return `-----BEGIN PRIVATE KEY-----\n${Buffer.from(pkcs8).toString("base64")}\n-----END PRIVATE KEY-----\n`;
}

test("a KV write over quota still sends, and later sends reuse the token", async () => {
  const config = { clientEmail: "relay@example.iam.gserviceaccount.com", privateKeyPem: await testPrivateKeyPem(), projectId: "kypost-relay-test" };
  const quotaCache = {
    get: async () => null,
    put: async () => {
      throw new Error("KV put() limit exceeded for the day.");
    },
    delete: async () => {},
  } as unknown as KVNamespace;
  let mints = 0;
  const realFetch = globalThis.fetch;
  globalThis.fetch = (async (url: string) => {
    if (url.startsWith("https://oauth2.googleapis.com/")) {
      mints++;
      return new Response(JSON.stringify({ access_token: "fresh-token", expires_in: 3600 }), { status: 200 });
    }
    return new Response("{}", { status: 200 });
  }) as typeof fetch;
  try {
    const message = { token: "t", title: "t", body: "b" };
    assert.equal((await sendFcmMessage(config, quotaCache, message)).ok, true);
    assert.equal((await sendFcmMessage(config, quotaCache, message)).ok, true);
  } finally {
    globalThis.fetch = realFetch;
  }
  assert.equal(mints, 1);
});
