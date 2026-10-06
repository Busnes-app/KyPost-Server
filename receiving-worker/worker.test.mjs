import test from "node:test";
import assert from "node:assert/strict";
import worker from "./worker.mjs";

const token = "12".repeat(32);
const raw = Uint8Array.from([83, 117, 98, 106, 101, 99, 116, 58, 32, 80, 71, 80, 13, 10, 13, 10, 0, 255, 128]);
const route = () => ({recipient: "mail-test@example.test", issuer: "https://identity.example.test", subject: "user-identity", mailbox: "immutable-owner", source: "native-source", revision: 1, validUntil: Math.floor(Date.now()/1000) + 900});
function fixture() {
  const MAIL = {
    object: null, reads: 0, writes: 0,
    async put(key, bytes, options) {
      assert.equal(key, "pilot-message");
      assert.equal(options.onlyIf.get("If-None-Match"), "*");
      this.writes++;
      if (this.object) return null;
      this.object = {raw: bytes.slice(), size: bytes.byteLength, customMetadata: options.customMetadata};
      return {key};
    },
    async get(key) {
      assert.equal(key, "pilot-message");
      this.reads++;
      return this.object && {...this.object, body: this.object.raw.slice()};
    },
    delete() { assert.fail("Provider deletion is prohibited in the pilot"); },
  };
  return {MAIL, PICKUP_TOKEN: token, ROUTE: JSON.stringify(route())};
}
function message(bytes = raw, overrides = {}) {
  return {from: "controlled@example.test", to: "mail-test@example.test", rawSize: bytes.byteLength, raw: new Blob([bytes]).stream(), rejected: [], setReject(reason) { this.rejected.push(reason); }, ...overrides};
}
function request(method = "GET", authorization = `Bearer ${token}`, path = "/message", protocol = "https") {
  return new Request(`${protocol}://pilot.operator.workers.dev${path}`, {method, headers: {Authorization: authorization}});
}

test("capture is awaited, binary exact, retained and idempotently downloadable", async () => {
  const env = fixture(), input = message();
  let finish;
  const original = env.MAIL.put.bind(env.MAIL);
  env.MAIL.put = async (...args) => { await new Promise(resolve => { finish = resolve; }); return original(...args); };
  let done = false;
  const capture = worker.email(input, env).then(() => { done = true; });
  while (!finish) await new Promise(resolve => setImmediate(resolve));
  assert.equal(done, false);
  assert.equal(env.MAIL.object, null);
  finish(); await capture;
  assert.deepEqual(input.rejected, []);
  const response = await worker.fetch(request(), env);
  assert.equal(response.status, 200);
  assert.equal(response.headers.get("Cache-Control"), "no-store");
  const metadata = JSON.parse(Buffer.from(response.headers.get("X-Kypost-Envelope"), "base64").toString());
  assert.match(metadata.id, /^[a-f0-9-]{36}$/);
  assert.deepEqual(metadata.route, JSON.parse(env.ROUTE));
  assert.equal(metadata.size, raw.length);
  assert.deepEqual(new Uint8Array(await response.arrayBuffer()), raw);
  env.ROUTE = JSON.stringify({...route(), validUntil: 1});
  const again = await worker.fetch(request(), env);
  assert.equal(again.headers.get("X-Kypost-Envelope"), response.headers.get("X-Kypost-Envelope"));
  assert.deepEqual(new Uint8Array(await again.arrayBuffer()), raw);
  assert.ok(env.MAIL.object);
});

test("concurrent or ambiguous captures never replace the first object", async () => {
  const env = fixture(), a = message(), b = message(Uint8Array.of(1,2,3));
  await Promise.all([worker.email(a, env), worker.email(b, env)]);
  assert.equal(a.rejected.length + b.rejected.length, 1);
  const first = env.MAIL.object;
  const original = env.MAIL.put.bind(env.MAIL);
  env.MAIL.put = async (...args) => { await original(...args); throw new Error("sensitive provider diagnostics"); };
  const third = message(); await worker.email(third, env);
  assert.equal(third.rejected.length, 1);
  assert.equal(env.MAIL.object, first);
  assert.ok(!third.rejected[0].includes("sensitive"));
});

test("invalid, expired, oversized and wrong-recipient capture has no writes", async () => {
  for (const variant of ["recipient", "expiry", "future", "raw", "size", "sender", "token", "claim"]) {
    const env = fixture(), input = message();
    if (variant === "recipient") input.to = "other@example.test";
    if (variant === "expiry") env.ROUTE = JSON.stringify({...route(), validUntil: 1});
    if (variant === "future") env.ROUTE = JSON.stringify({...route(), validUntil: Math.floor(Date.now()/1000)+901});
    if (variant === "raw") { input.raw = new Blob([new Uint8Array((4<<20)+1)]).stream(); input.rawSize = 1; }
    if (variant === "size") input.rawSize = (4<<20)+1;
    if (variant === "sender") input.from = "sender\r\nInjected: yes";
    if (variant === "token") env.PICKUP_TOKEN = "";
    if (variant === "claim") env.ROUTE = "{}";
    await worker.email(input, env);
    assert.equal(input.rejected.length, 1, variant);
    assert.equal(env.MAIL.writes, 0, variant);
  }
});

test("authentication precedes all reads and HTTP cannot delete", async () => {
  const env = fixture(); await worker.email(message(), env);
  for (const auth of ["", "Bearer wrong", `Bearer ${"ff".repeat(32)}`, `bearer ${token}`]) {
    assert.equal((await worker.fetch(request("GET", auth), env)).status, 401);
  }
  assert.equal((await worker.fetch(request("GET", `Bearer ${token}`, "/message", "http"), env)).status, 401);
  assert.equal(env.MAIL.reads, 0);
  for (const method of ["DELETE", "POST", "PUT"]) assert.equal((await worker.fetch(request(method), env)).status, 404);
  assert.equal((await worker.fetch(request("GET", `Bearer ${token}`, "/message?recipient=other"), env)).status, 404);
  assert.equal(env.MAIL.reads, 0);
  assert.ok(env.MAIL.object);
});

test("storage failure refuses and pickup error is sanitized", async () => {
  const env = fixture(), input = message();
  env.MAIL.put = async () => { throw new Error("private correspondence"); };
  await worker.email(input, env);
  assert.equal(input.rejected.length, 1);
  env.MAIL.get = async () => { throw new Error("private correspondence"); };
  const response = await worker.fetch(request(), env);
  assert.equal(response.status, 503);
  assert.equal(await response.text(), "");
});
