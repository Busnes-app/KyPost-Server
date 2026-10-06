// Disposable real workerd/R2 proof using the existing pinned Wrangler dependency.
import {createRequire} from "node:module";
import {readFile, writeFile, mkdtemp, rm} from "node:fs/promises";
import {tmpdir} from "node:os";
import {join} from "node:path";
import assert from "node:assert/strict";
const require = createRequire(new URL("../worker/package.json", import.meta.url));
const {Miniflare, convertV4MiniflareOptions} = require("miniflare");
const dir = await mkdtemp(join(tmpdir(), "kypost-cf-runtime-"));
const token = "ab".repeat(32); // Synthetic test credential, never used live.
const route = {recipient: "one@example.test", issuer: "https://identity.example.test", subject: "one", mailbox: "immutable-owner", source: "native-source", revision: 1, validUntil: Math.floor(Date.now()/1000)+900};
let mf;
try {
  await writeFile(join(dir, "worker.mjs"), await readFile(new URL("./worker.mjs", import.meta.url)));
  await writeFile(join(dir, "driver.mjs"), `import worker from './worker.mjs'; export default {async fetch(req,env){if(new URL(req.url).pathname==='/capture'){const rejected=[];await worker.email({from:'sender@example.test',to:'one@example.test',raw:req.body,rawSize:0,setReject(v){rejected.push(v)}},env);return Response.json({rejected});}return worker.fetch(req,env)}}`);
  mf = new Miniflare(convertV4MiniflareOptions({modules: [{type: "ESModule", path: join(dir, "driver.mjs")}, {type: "ESModule", path: join(dir, "worker.mjs")}], modulesRoot: dir, compatibilityDate: "2026-09-18", r2Buckets: ["MAIL"], r2Persist: join(dir, "r2"), bindings: {ROUTE: JSON.stringify(route), PICKUP_TOKEN: token}, host: "127.0.0.1", port: 0}));
  const origin = "https://pilot.operator.workers.dev";
  const raw = Uint8Array.of(83,117,98,106,101,99,116,58,32,65,13,10,13,10,0,255,128);
  const captures = await Promise.all([raw, "replacement"].map(body => mf.dispatchFetch(origin+"/capture", {method: "POST", body}).then(r => r.json())));
  assert.equal(captures.filter(r => r.rejected.length === 0).length, 1);
  assert.equal(captures.filter(r => r.rejected.length === 1).length, 1);
  const result = await mf.dispatchFetch(origin+"/message", {headers: {Authorization: "Bearer "+token}});
  assert.equal(result.status, 200);
  const stored = new Uint8Array(await result.arrayBuffer());
  const expected = captures[0].rejected.length === 0 ? raw : new TextEncoder().encode("replacement");
  assert.deepEqual(stored, expected);
  const bucket = await mf.getR2Bucket("MAIL");
  assert.equal((await bucket.list()).objects.length, 1);
  const object = await bucket.get("pilot-message");
  assert.equal(JSON.parse(object.customMetadata.envelope).size, stored.length);
  const again = await mf.dispatchFetch(origin+"/message", {headers: {Authorization: "Bearer "+token}});
  assert.equal(again.headers.get("X-Kypost-Envelope"), result.headers.get("X-Kypost-Envelope"));
  assert.deepEqual(new Uint8Array(await again.arrayBuffer()), stored);
  console.log("PASS: workerd/R2 atomic capture, occupied refusal, exact pickup replay and retained object");
} finally {
  if (mf) await mf.dispose();
  await rm(dir, {recursive: true, force: true});
}
