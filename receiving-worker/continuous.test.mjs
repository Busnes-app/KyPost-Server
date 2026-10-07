import test from "node:test";
import assert from "node:assert/strict";
import {createHash} from "node:crypto";
import worker, {contexts, limits} from "./continuous.mjs";

const enc = new TextEncoder();
const b64 = bytes => Buffer.from(bytes).toString("base64");
const sha = text => createHash("sha256").update(text).digest("hex");
const token = (n) => n.toString(16).padStart(2, "0").repeat(32); // Synthetic test credentials.
const DAY = 86400_000;

async function keypair() {
  const pair = await crypto.subtle.generateKey({name: "Ed25519"}, true, ["sign", "verify"]);
  return {...pair, public: b64(new Uint8Array(await crypto.subtle.exportKey("raw", pair.publicKey)))};
}
async function sign(key, context, text) {
  return b64(new Uint8Array(await crypto.subtle.sign({name: "Ed25519"}, key.privateKey, enc.encode(context + text))));
}
async function signedTable(key, table) {
  const text = JSON.stringify(table);
  return JSON.stringify({table: text, signature: await sign(key, contexts.routes, text)});
}
async function signedRotation(key, rotation) {
  const text = JSON.stringify(rotation);
  return JSON.stringify({rotation: text, signature: await sign(key, contexts.rotate, text)});
}
const table = (over = {}) => ({
  revision: Date.now(), issuedAt: Date.now(),
  routes: [{address: "one@example.test", generation: 3, maxBytes: 1024}, {address: "two@other.test", generation: 1, maxBytes: 64}],
  blockedSenders: [], ...over,
});

// In-memory R2: conditional put via Headers, ordered list, per-key read log.
function bucket() {
  let n = 0;
  const objects = new Map(), log = [];
  const view = (key, o, body) => o && {key, size: o.bytes.byteLength, httpEtag: `"${o.etag}"`, customMetadata: {...o.meta},
    ...(body && {body: new Blob([o.bytes]).stream(), text: async () => new TextDecoder().decode(o.bytes)})};
  return {
    objects, log, failPut: null,
    reads(except = "credentials.json") { return log.filter(([op, key]) => op !== "put" && key !== except); },
    async get(key) { log.push(["get", key]); return view(key, objects.get(key), true); },
    async head(key) { log.push(["head", key]); return view(key, objects.get(key), false); },
    async put(key, value, options = {}) {
      log.push(["put", key]);
      if (this.failPut) throw this.failPut;
      await null; // Let concurrent callers interleave like a real store.
      const current = objects.get(key), cond = options.onlyIf;
      if (cond?.get("If-None-Match") === "*" && current) return null;
      if (cond?.get("If-Match") && (!current || cond.get("If-Match") !== `"${current.etag}"`)) return null;
      const bytes = typeof value === "string" ? enc.encode(value) : value.slice();
      if (options.sha256) assert.equal(options.sha256, sha(bytes));
      objects.set(key, {bytes, etag: `e${++n}`, meta: options.customMetadata ?? {}});
      return {key};
    },
    async delete(key) { log.push(["delete", key]); objects.delete(key); },
    async list({prefix, startAfter, limit, include}) {
      log.push(["list", prefix]);
      assert.deepEqual(include, ["customMetadata"]);
      const keys = [...objects.keys()].filter(k => k.startsWith(prefix) && (!startAfter || k > startAfter)).sort();
      return {objects: keys.slice(0, limit).map(k => view(k, objects.get(k), false)), truncated: keys.length > limit};
    },
  };
}

async function fixture() {
  const key = await keypair();
  return {key, env: {MAIL: bucket(), PICKUP_TOKEN_SHA256: sha(token(1)), ROUTING_PUBLIC_KEY: key.public}};
}
function call(env, method, path, {bearer = token(1), body, protocol = "https"} = {}) {
  return worker.fetch(new Request(`${protocol}://receiving.operator.workers.dev${path}`,
    {method, body, headers: bearer === null ? {} : {Authorization: `Bearer ${bearer}`}}), env);
}
async function install(f, t = table()) {
  return (await call(f.env, "PUT", "/routes", {body: await signedTable(f.key, t)})).status;
}
function mail(over = {}, bytes = enc.encode("Subject: hi\r\n\r\n\x00body")) {
  return {from: "Sender@Example.TEST", to: "ONE@example.test", rawSize: bytes.byteLength,
    raw: new Blob([bytes]).stream(), rejected: [], setReject(r) { this.rejected.push(r); }, ...over};
}
const inbox = env => [...env.MAIL.objects.keys()].filter(k => k.startsWith("inbox/"));
// Hold the first n puts to key until all arrive, so racing writers share one pre-write read.
function gate(env, key, n = 2) {
  const put = env.MAIL.put.bind(env.MAIL), waiting = [];
  env.MAIL.put = async (...args) => {
    if (args[0] !== key || waiting.length >= n) return put(...args);
    await new Promise(resolve => { waiting.push(resolve); if (waiting.length === n) waiting.forEach(r => r()); });
    return put(...args);
  };
}

test("capture persists exact bytes and the frozen envelope; pickup lists, fetches and digest-deletes", async () => {
  const f = await fixture();
  assert.equal(await install(f), 204);
  const bytes = enc.encode("Subject: hi\r\n\r\n\x00\xff");
  const m = mail({}, bytes);
  await worker.email(m, f.env);
  assert.deepEqual(m.rejected, []);
  const [key] = inbox(f.env);
  assert.match(key, /^inbox\/[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  const id = key.slice(6);

  const list = await (await call(f.env, "GET", "/messages")).json();
  assert.deepEqual(list, {messages: [{key: id, size: bytes.byteLength, digest: sha(bytes)}], truncated: false});

  const got = await call(f.env, "GET", `/messages/${id}`);
  assert.equal(got.status, 200);
  assert.equal(got.headers.get("Cache-Control"), "no-store");
  const envelope = JSON.parse(Buffer.from(got.headers.get("X-KyPost-Envelope"), "base64url").toString());
  assert.deepEqual(Object.keys(envelope), ["id", "sender", "recipient", "generation", "tableRevision", "capturedAt", "size", "digest"]);
  assert.equal(envelope.id, id);
  assert.equal(envelope.sender, "Sender@example.test");
  assert.equal(envelope.recipient, "one@example.test");
  assert.equal(envelope.generation, 3);
  assert.equal(envelope.digest, sha(bytes));
  assert.deepEqual(new Uint8Array(await got.arrayBuffer()), bytes);

  assert.equal((await call(f.env, "DELETE", `/messages/${id}?digest=${"0".repeat(64)}`)).status, 409);
  assert.equal(inbox(f.env).length, 1);
  assert.equal((await call(f.env, "DELETE", `/messages/${id}?digest=${sha(bytes)}`)).status, 204);
  assert.equal(inbox(f.env).length, 0);
  assert.equal((await call(f.env, "DELETE", `/messages/${id}?digest=${sha(bytes)}`)).status, 204, "absent is success");
  assert.equal((await call(f.env, "GET", `/messages/${id}`)).status, 404);
});

test("refusals happen before the body is read and store nothing", async () => {
  const f = await fixture();
  const now = Date.now();
  await install(f, table({blockedSenders: [
    {address: "bad@spam.test", until: null}, {domain: "evil.test", until: now + DAY}, {domain: "expired.test", until: now - 1},
  ]}));
  const cases = {
    "blocked address": mail({from: "bad@SPAM.test"}),
    "blocked domain": mail({from: "anyone@Evil.Test"}),
    "unknown address": mail({to: "nobody@example.test"}),
    "absent domain": mail({to: "one@absent.test"}),
    "announced size": mail({to: "two@other.test", rawSize: 65}),
    "bad recipient": mail({to: "one@exa mple.test"}),
    "injected sender": mail({from: "x\r\n@example.test"}),
  };
  for (const [name, m] of Object.entries(cases)) {
    let read = false;
    Object.defineProperty(m, "raw", {get() { read = true; return new Blob([]).stream(); }});
    await worker.email(m, f.env);
    assert.equal(m.rejected.length, 1, name);
    assert.equal(read, false, name);
  }
  const lies = mail({to: "two@other.test", rawSize: 10}, new Uint8Array(65));
  await worker.email(lies, f.env);
  assert.deepEqual(lies.rejected, ["Message too large"]);
  const expired = mail({from: "someone@expired.test"});
  await worker.email(expired, f.env);
  assert.deepEqual(expired.rejected, [], "an expired block no longer applies");
  const bounce = mail({from: ""});
  await worker.email(bounce, f.env);
  assert.deepEqual(bounce.rejected, []);
  assert.equal(inbox(f.env).length, 2);
});

test("no table or a 14-day-old table refuses", async () => {
  const f = await fixture();
  const none = mail();
  await worker.email(none, f.env);
  assert.equal(none.rejected.length, 1);
  await install(f, table({revision: Date.now() - 14 * DAY + 60_000, issuedAt: Date.now() - 14 * DAY + 60_000}));
  const fresh = mail();
  await worker.email(fresh, f.env);
  assert.deepEqual(fresh.rejected, []);
  const stored = JSON.parse(new TextDecoder().decode(f.env.MAIL.objects.get("routes.json").bytes));
  const old = {...JSON.parse(stored.table), issuedAt: Date.now() - 14 * DAY - 1};
  f.env.MAIL.objects.get("routes.json").bytes = enc.encode(await signedTable(f.key, old));
  const stale = mail();
  await worker.email(stale, f.env);
  assert.deepEqual(stale.rejected, ["Routing unavailable"]);
  assert.equal(inbox(f.env).length, 1);
});

test("capture resolves only after the conditional put", async () => {
  const f = await fixture();
  await install(f);
  const put = f.env.MAIL.put.bind(f.env.MAIL);
  let release;
  f.env.MAIL.put = async (...args) => { await new Promise(r => { release = r; }); return put(...args); };
  let done = false;
  const capture = worker.email(mail(), f.env).then(() => { done = true; });
  for (let i = 0; !release && i < 1000; i++) await new Promise(r => setImmediate(r));
  assert.ok(release, "put was reached");
  assert.equal(done, false);
  release();
  await capture;
  assert.equal(inbox(f.env).length, 1);
});

test("capture never overwrites and storage failure throws instead of rejecting", async () => {
  const f = await fixture();
  await install(f);
  const put = f.env.MAIL.put.bind(f.env.MAIL);
  f.env.MAIL.objects.set("inbox/x", {bytes: new Uint8Array(1), etag: "x", meta: {}});
  f.env.MAIL.put = async (key, value, options) => put("inbox/x", value, options); // Force a key collision.
  const collide = mail();
  await assert.rejects(worker.email(collide, f.env));
  assert.deepEqual(collide.rejected, []);
  assert.equal(f.env.MAIL.objects.get("inbox/x").bytes.byteLength, 1);
  f.env.MAIL.put = put;
  f.env.MAIL.failPut = new Error("private correspondence");
  const failed = mail();
  await assert.rejects(worker.email(failed, f.env));
  assert.deepEqual(failed.rejected, []);
});

test("table install verifies signature, ordering, clock bounds, shape and size", async () => {
  const f = await fixture();
  const now = Date.now();
  const t = table({revision: now - 1000});
  assert.equal(await install(f, t), 204);
  const text = JSON.stringify(table({revision: now}));
  const tampered = JSON.stringify({table: text.replace("one@", "evil@"), signature: await sign(f.key, contexts.routes, text)});
  assert.equal((await call(f.env, "PUT", "/routes", {body: tampered})).status, 403);
  const wrongContext = JSON.stringify({table: text, signature: await sign(f.key, contexts.rotate, text)});
  assert.equal((await call(f.env, "PUT", "/routes", {body: wrongContext})).status, 403);
  const other = await keypair();
  assert.equal((await call(f.env, "PUT", "/routes", {body: await signedTable(other, table({revision: now}))})).status, 403);
  assert.equal(await install(f, t), 409, "replayed revision");
  assert.equal(await install(f, table({revision: now - 2000})), 409, "older revision");
  assert.equal(await install(f, table({revision: now + 6 * 60_000})), 422, "future revision");
  assert.equal(await install(f, table({revision: now, issuedAt: now - 15 * DAY})), 422, "already stale");
  const many = Array.from({length: limits.routes + 1}, (_, i) => ({address: `u${i}@example.test`, generation: 1, maxBytes: 1}));
  assert.equal(await install(f, table({revision: now, routes: many})), 400, "too many routes");
  const dup = [{address: "a@x.test", generation: 1, maxBytes: 1}, {address: "a@x.test", generation: 2, maxBytes: 1}];
  assert.equal(await install(f, table({revision: now, routes: dup})), 400, "duplicate address");
  assert.equal(await install(f, table({revision: now, routes: [{address: "A@x.test", generation: 1, maxBytes: 1}]})), 400, "uppercase");
  assert.equal(await install(f, table({revision: now, routes: [{address: "a@x.test", generation: 1, maxBytes: limits.messageBytes + 1}]})), 400);
  assert.equal(await install(f, table({revision: now, blockedSenders: [{address: "a@x.test", domain: "x.test", until: null}]})), 400);
  assert.equal(await install(f, table({revision: now, extra: 1})), 400);
  const huge = "x".repeat(2 * limits.tableBytes + 4097);
  assert.equal((await call(f.env, "PUT", "/routes", {body: huge})).status, 413);
  const stored = await (await call(f.env, "GET", "/routes")).json();
  assert.equal(JSON.parse(stored.table).revision, t.revision);
  assert.equal(await install(f, table({revision: now})), 204);
});

test("concurrent table installs: one wins by etag", async () => {
  const f = await fixture();
  await install(f, table({revision: Date.now() - 10}));
  gate(f.env, "routes.json");
  const statuses = await Promise.all([install(f, table({revision: Date.now() - 5})), install(f, table({revision: Date.now()}))]);
  assert.ok(statuses.includes(204));
  assert.equal(statuses.filter(s => s === 204).length, 1, String(statuses));
});

test("bearer is checked before any read; strict paths and HTTPS", async () => {
  const f = await fixture();
  await install(f);
  await worker.email(mail(), f.env);
  const id = inbox(f.env)[0].slice(6);
  f.env.MAIL.log.length = 0;
  const paths = [["GET", "/messages"], ["GET", `/messages/${id}`], ["DELETE", `/messages/${id}?digest=${"0".repeat(64)}`],
    ["GET", "/routes"], ["PUT", "/routes"], ["POST", "/rotate"]];
  for (const bearer of [null, token(2), "A" + token(1).slice(1), "abc"]) {
    for (const [method, path] of paths) assert.equal((await call(f.env, method, path, {bearer})).status, 401, `${method} ${path}`);
  }
  assert.equal((await call(f.env, "GET", "/messages", {protocol: "http"})).status, 403);
  assert.deepEqual(f.env.MAIL.reads(), [], "only credentials.json may be read before authentication");
  for (const [method, path] of [["GET", "/"], ["GET", "/message"], ["GET", "/messages/"], ["GET", `/messages/${id.toUpperCase()}`],
    ["GET", `/messages/${id}/`], ["GET", `/messages/x${id}`], ["GET", `/messages/${id}0`], ["GET", `/messages/${id}?x=1`], ["GET", "/messages?x=1"], ["GET", "/messages?after=a&after=b"],
    ["POST", "/messages"], ["DELETE", `/messages/${id}`], ["DELETE", `/messages/${id}?digest=zz`], ["GET", "/routes?x"],
    ["GET", "/rotate"], ["GET", "/credentials.json"], ["GET", "/messages/00000000-0000-4000-8000-000000000000"]]) {
    assert.equal((await call(f.env, method, path)).status, 404, `${method} ${path}`);
  }
  assert.equal((await call(f.env, "GET", "/messages?limit=101")).status, 400);
  assert.equal((await call(f.env, "GET", "/messages?after=nope")).status, 400);
  assert.equal((await call(f.env, "GET", `/messages?after=x${id}`)).status, 400);
});

test("listing pages in key (time) order with after and limit", async () => {
  const f = await fixture();
  await install(f);
  for (let i = 0; i < 5; i++) {
    await worker.email(mail(), f.env);
    await new Promise(r => setTimeout(r, 2)); // Distinct milliseconds keep uuidv7 order strict.
  }
  const all = inbox(f.env).map(k => k.slice(6));
  const page1 = await (await call(f.env, "GET", "/messages?limit=2")).json();
  assert.deepEqual(page1.messages.map(m => m.key), all.slice(0, 2));
  assert.equal(page1.truncated, true);
  const page2 = await (await call(f.env, "GET", `/messages?after=${all[1]}&limit=100`)).json();
  assert.deepEqual(page2.messages.map(m => m.key), all.slice(2));
  assert.equal(page2.truncated, false);
  const created = all.map(k => parseInt(k.replace("-", "").slice(0, 12), 16));
  assert.deepEqual(created, [...created].sort((a, b) => a - b));
});

test("rotate: current signer, epoch+1, fences the old bearer, concurrent loser refused", async () => {
  const f = await fixture();
  await install(f);
  const next = await keypair();
  const rotation = {epoch: 2, tokenSha256: sha(token(2)), publicKey: next.public};
  assert.equal((await call(f.env, "POST", "/rotate", {body: await signedRotation(next, rotation)})).status, 403, "wrong signer");
  const asTable = JSON.stringify({rotation: JSON.stringify(rotation), signature: await sign(f.key, contexts.routes, JSON.stringify(rotation))});
  assert.equal((await call(f.env, "POST", "/rotate", {body: asTable})).status, 403, "routes signature is not a rotate signature");
  assert.equal((await call(f.env, "POST", "/rotate", {body: await signedRotation(f.key, {...rotation, epoch: 3})})).status, 409);
  assert.equal((await call(f.env, "POST", "/rotate", {body: await signedRotation(f.key, {...rotation, epoch: 1})})).status, 409);
  assert.equal((await call(f.env, "POST", "/rotate", {body: await signedRotation(f.key, {...rotation, tokenSha256: "x"})})).status, 400);

  const rival = {epoch: 2, tokenSha256: sha(token(3)), publicKey: (await keypair()).public};
  gate(f.env, "credentials.json");
  const [a, b] = await Promise.all([
    call(f.env, "POST", "/rotate", {body: await signedRotation(f.key, rotation)}),
    call(f.env, "POST", "/rotate", {body: await signedRotation(f.key, rival)}),
  ]);
  assert.deepEqual([a.status, b.status].sort(), [204, 409]);
  const winner = a.status === 204 ? 2 : 3, winnerKey = a.status === 204 ? next : null;
  assert.equal(JSON.parse(new TextDecoder().decode(f.env.MAIL.objects.get("credentials.json").bytes)).epoch, 2);

  for (const [method, path] of [["GET", "/messages"], ["GET", "/routes"], ["PUT", "/routes"], ["POST", "/rotate"]]) {
    assert.equal((await call(f.env, method, path)).status, 401, `old bearer ${method} ${path}`);
  }
  assert.equal((await call(f.env, "GET", "/messages", {bearer: token(winner)})).status, 200);
  assert.equal((await call(f.env, "POST", "/rotate", {bearer: token(winner), body: await signedRotation(f.key, {...rotation, epoch: 3})})).status, 403, "old key cannot rotate again");
  if (winnerKey) {
    assert.equal((await call(f.env, "PUT", "/routes", {bearer: token(2), body: await signedTable(f.key, table())})).status, 403, "old key cannot sign tables");
    assert.equal((await call(f.env, "PUT", "/routes", {bearer: token(2), body: await signedTable(next, table({revision: Date.now() + 1000}))})).status, 204);
    const third = {epoch: 3, tokenSha256: sha(token(4)), publicKey: next.public};
    assert.equal((await call(f.env, "POST", "/rotate", {bearer: token(2), body: await signedRotation(next, third)})).status, 204);
    assert.equal((await call(f.env, "GET", "/messages", {bearer: token(2)})).status, 401);
  }
  f.env.PICKUP_TOKEN_SHA256 = sha(token(1));
  assert.equal((await call(f.env, "GET", "/messages")).status, 401, "bootstrap secrets do not override credentials.json");
});

test("invalid bootstrap secrets and storage errors fail closed without detail", async () => {
  const f = await fixture();
  f.env.ROUTING_PUBLIC_KEY = "";
  assert.equal((await call(f.env, "GET", "/messages")).status, 503);
  const g = await fixture();
  g.env.MAIL.list = async () => { throw new Error("private correspondence"); };
  const r = await call(g.env, "GET", "/messages");
  assert.equal(r.status, 503);
  assert.equal(await r.text(), "");
});
