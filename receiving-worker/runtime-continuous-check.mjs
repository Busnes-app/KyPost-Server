// Disposable real workerd/R2 proof of the continuous protocol, using the pinned
// Wrangler/Miniflare dependency in worker/. Synthetic keys only, never used live.
import {createRequire} from "node:module";
import {createHash} from "node:crypto";
import {readFile, writeFile, mkdtemp, rm} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import assert from "node:assert/strict";
const require = createRequire(new URL("../worker/package.json", import.meta.url));
const {Miniflare, convertV4MiniflareOptions} = require("miniflare");

const enc = new TextEncoder();
const b64 = bytes => Buffer.from(bytes).toString("base64");
const sha = data => createHash("sha256").update(data).digest("hex");
const tokens = ["ab".repeat(32), "cd".repeat(32), "ef".repeat(32)];
const keys = await Promise.all([0, 1, 2].map(() => crypto.subtle.generateKey({name: "Ed25519"}, true, ["sign", "verify"])));
const publicKey = async k => b64(new Uint8Array(await crypto.subtle.exportKey("raw", k.publicKey)));
async function signed(field, context, key, value) {
  const text = JSON.stringify(value);
  const signature = b64(new Uint8Array(await crypto.subtle.sign({name: "Ed25519"}, key.privateKey, enc.encode(context + text))));
  return JSON.stringify({[field]: text, signature});
}
const table = revision => ({revision, issuedAt: revision, routes: [{address: "one@example.test", generation: 7, maxBytes: 1024}], blockedSenders: [{domain: "blocked.test", until: null}]});

const dir = await mkdtemp(join(tmpdir(), "kypost-cf-continuous-"));
let mf;
try {
  await writeFile(join(dir, "continuous.mjs"), await readFile(new URL("./continuous.mjs", import.meta.url)));
  // The driver turns POST /capture?from=&to= into an email event; everything else is the real fetch handler.
  await writeFile(join(dir, "driver.mjs"), `import worker from './continuous.mjs';
export default {async fetch(req, env) {
  const url = new URL(req.url);
  if (url.pathname !== '/capture') return worker.fetch(req, env);
  const rejected = [];
  try { await worker.email({from: url.searchParams.get('from'), to: url.searchParams.get('to'), raw: req.body, rawSize: Number(req.headers.get('x-size')), setReject(v) { rejected.push(v); }}, env); }
  catch { return Response.json({thrown: true}); }
  return Response.json({rejected});
}}`);
  mf = new Miniflare(convertV4MiniflareOptions({
    modules: [{type: "ESModule", path: join(dir, "driver.mjs")}, {type: "ESModule", path: join(dir, "continuous.mjs")}],
    modulesRoot: dir, compatibilityDate: "2026-09-18", r2Buckets: ["MAIL"], r2Persist: join(dir, "r2"),
    bindings: {PICKUP_TOKEN_SHA256: sha(tokens[0]), ROUTING_PUBLIC_KEY: await publicKey(keys[0])}, host: "127.0.0.1", port: 0,
  }));
  const origin = "https://receiving.operator.workers.dev";
  const api = (method, path, token, body) => mf.dispatchFetch(origin + path, {method, body, headers: {Authorization: "Bearer " + token}});
  const capture = (to, body, from = "sender@example.test") =>
    mf.dispatchFetch(`${origin}/capture?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`, {method: "POST", body, headers: {"x-size": String(body.byteLength)}}).then(r => r.json());
  const raw = Uint8Array.of(83, 117, 98, 106, 101, 99, 116, 58, 32, 65, 13, 10, 13, 10, 0, 255, 128);

  // Signed table install in workerd's Ed25519, with ordering and tamper refusal.
  assert.deepEqual((await capture("one@example.test", raw)).rejected.length, 1, "no table refuses");
  const now = Date.now();
  assert.equal((await api("PUT", "/routes", tokens[0], await signed("table", "kypost-cf-routes/1\n", keys[1], table(now)))).status, 403);
  assert.equal((await api("PUT", "/routes", tokens[0], await signed("table", "kypost-cf-routes/1\n", keys[0], table(now)))).status, 204);
  assert.equal((await api("PUT", "/routes", tokens[0], await signed("table", "kypost-cf-routes/1\n", keys[0], table(now)))).status, 409);
  const installs = await Promise.all([now + 1, now + 2].map(async r => (await api("PUT", "/routes", tokens[0], await signed("table", "kypost-cf-routes/1\n", keys[0], table(r)))).status));
  assert.ok(installs.includes(204), String(installs));

  // Conditional capture, refusals, list/get/digest-checked delete against real R2.
  assert.equal((await capture("nobody@example.test", raw)).rejected.length, 1);
  assert.equal((await capture("one@example.test", raw, "x@Blocked.Test")).rejected.length, 1);
  assert.equal((await capture("one@example.test", new Uint8Array(1025))).rejected.length, 1);
  const accepted = await Promise.all([capture("ONE@example.test", raw), capture("one@example.test", raw)]);
  assert.deepEqual(accepted, [{rejected: []}, {rejected: []}]);
  const bucket = await mf.getR2Bucket("MAIL");
  const stored = (await bucket.list({prefix: "inbox/"})).objects;
  assert.equal(stored.length, 2);
  const conflict = await bucket.put(stored[0].key, "overwrite", {onlyIf: new Headers({"If-None-Match": "*"})});
  assert.equal(conflict, null, "R2 refuses to overwrite a captured key");

  const listed = await (await api("GET", "/messages?limit=1", tokens[0])).json();
  assert.equal(listed.messages.length, 1);
  assert.equal(listed.truncated, true);
  const rest = await (await api("GET", `/messages?after=${listed.messages[0].key}`, tokens[0])).json();
  assert.equal(rest.messages.length, 1);
  assert.ok(rest.messages[0].key > listed.messages[0].key);
  const [{key, digest, size}] = listed.messages;
  assert.equal(digest, sha(raw));
  assert.equal(size, raw.byteLength);
  const got = await api("GET", `/messages/${key}`, tokens[0]);
  const envelope = JSON.parse(Buffer.from(got.headers.get("X-KyPost-Envelope"), "base64url").toString());
  assert.equal(envelope.recipient, "one@example.test");
  assert.equal(envelope.generation, 7);
  assert.deepEqual(new Uint8Array(await got.arrayBuffer()), raw);
  assert.equal((await api("DELETE", `/messages/${key}?digest=${"0".repeat(64)}`, tokens[0])).status, 409);
  assert.equal((await api("DELETE", `/messages/${key}?digest=${digest}`, tokens[0])).status, 204);
  assert.equal((await api("DELETE", `/messages/${key}?digest=${digest}`, tokens[0])).status, 204);
  assert.equal((await bucket.list({prefix: "inbox/"})).objects.length, 1);

  // Rotation fencing: a concurrent rival loses on the etag; the old bearer and key are refused.
  const candidates = await Promise.all([1, 2].map(async i => ({epoch: 2, tokenSha256: sha(tokens[i]), publicKey: await publicKey(keys[i])})));
  const rotations = await Promise.all(candidates.map(async r => (await api("POST", "/rotate", tokens[0], await signed("rotation", "kypost-cf-rotate/1\n", keys[0], r))).status));
  // The loser is refused by the etag (409) or, if it authenticated after the winner wrote, by the bearer (401).
  assert.equal(rotations.filter(s => s === 204).length, 1, String(rotations));
  assert.ok(rotations.every(s => [204, 401, 409].includes(s)), String(rotations));
  const stored2 = JSON.parse(await (await bucket.get("credentials.json")).text());
  assert.equal(stored2.epoch, 2);
  const w = rotations.indexOf(204) + 1; // Either order is valid; continue with the winner.
  assert.equal(stored2.tokenSha256, sha(tokens[w]));
  for (const [method, path] of [["GET", "/messages"], ["GET", `/messages/${rest.messages[0].key}`], ["DELETE", `/messages/${rest.messages[0].key}?digest=${rest.messages[0].digest}`], ["PUT", "/routes"]]) {
    assert.equal((await api(method, path, tokens[0])).status, 401, `old bearer ${method}`);
  }
  assert.equal((await api("GET", "/messages", tokens[3 - w])).status, 401, "rival bearer refused");
  assert.equal((await api("GET", "/messages", tokens[w])).status, 200);
  assert.equal((await api("PUT", "/routes", tokens[w], await signed("table", "kypost-cf-routes/1\n", keys[0], table(Date.now() + 10)))).status, 403);
  assert.equal((await api("PUT", "/routes", tokens[w], await signed("table", "kypost-cf-routes/1\n", keys[w], table(Date.now() + 10)))).status, 204);
  assert.equal((await bucket.list({prefix: "inbox/"})).objects.length, 1, "old holder deleted nothing");
  // The If-Match replace path the Worker relies on: a stale etag is refused, the current one succeeds.
  const current = await bucket.head("credentials.json");
  assert.equal(await bucket.put("credentials.json", "{}", {onlyIf: new Headers({"If-Match": '"0000"'})}), null);
  const third = {epoch: 3, tokenSha256: sha(tokens[0]), publicKey: await publicKey(keys[0])};
  assert.equal((await api("POST", "/rotate", tokens[w], await signed("rotation", "kypost-cf-rotate/1\n", keys[w], third))).status, 204);
  assert.notEqual((await bucket.head("credentials.json")).httpEtag, current.httpEtag);
  assert.equal((await api("GET", "/messages", tokens[w])).status, 401);
  console.log("PASS: workerd/R2 signed table install, conditional capture, list/get/digest delete and rotation fencing");
} finally {
  if (mf) await mf.dispose();
  await rm(dir, {recursive: true, force: true});
}
