// Continuous receiving: signed routing table, R2 queue, authenticated pickup.
// Wire contract: docs/CLOUDFLARE_CONTINUOUS_RECEIVING.md#wire-contract.
// Self-contained so the pilot (worker.mjs) can be deleted without touching this file.
const MiB = 1 << 20;
export const limits = {
  messageBytes: 25 * MiB, tableBytes: MiB, routes: 5000, blocks: 5000, rotateBody: 4096,
  envelopeBytes: 2000, maxAgeMs: 14 * 86400_000, futureMs: 5 * 60_000, page: 100,
};
export const contexts = {routes: "kypost-cf-routes/1\n", rotate: "kypost-cf-rotate/1\n"};
const ROUTES = "routes.json", CREDENTIALS = "credentials.json", INBOX = "inbox/";
const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;
const HEX64 = /^[0-9a-f]{64}$/;
const enc = new TextEncoder();
const hex = bytes => Array.from(new Uint8Array(bytes), b => b.toString(16).padStart(2, "0")).join("");
const sha256 = async bytes => new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
const isInt = (v, min = 1) => Number.isSafeInteger(v) && v >= min;
const exactKeys = (o, keys) => o !== null && typeof o === "object" && !Array.isArray(o) &&
  Object.keys(o).length === keys.length && keys.every(k => Object.hasOwn(o, k));
const response = (status, body = null, headers = {}) =>
  new Response(body, {status, headers: {"Cache-Control": "no-store", ...headers}});

// Lowercase printable ASCII; domains are A-labels, so a U-label cannot dodge a block.
const domainOk = d => typeof d === "string" && d.length <= 253 && d === d.toLowerCase() && /^[\x21-\x3f\x41-\x7e]+$/.test(d);
const addressOk = a => typeof a === "string" && a.length <= 254 && a === a.toLowerCase() &&
  /^[\x21-\x3f\x41-\x7e]{1,64}@[\x21-\x3f\x41-\x7e]+$/.test(a);

function base64(text, length) {
  if (typeof text !== "string" || !/^[A-Za-z0-9+/]+={0,2}$/.test(text)) return null;
  const bytes = Uint8Array.from(atob(text), c => c.charCodeAt(0));
  return bytes.length === length && btoa(String.fromCharCode(...bytes)) === text ? bytes : null;
}
const base64url = bytes => btoa(String.fromCharCode(...bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

export function uuidv7(now) {
  const b = crypto.getRandomValues(new Uint8Array(16));
  for (let i = 5, t = now; i >= 0; i--, t = Math.floor(t / 256)) b[i] = t % 256;
  b[6] = (b[6] & 0x0f) | 0x70;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = hex(b);
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

// Returns the bytes, or null when the stream exceeds limit.
async function readBounded(stream, limit) {
  if (!stream) return new Uint8Array();
  const reader = stream.getReader(), chunks = [];
  let size = 0;
  try {
    for (;;) {
      const {value, done} = await reader.read();
      if (done) break;
      if ((size += value.byteLength) > limit) { await reader.cancel(); return null; }
      chunks.push(value);
    }
  } finally { reader.releaseLock(); }
  const out = new Uint8Array(size);
  let offset = 0;
  for (const c of chunks) { out.set(c, offset); offset += c.byteLength; }
  return out;
}

// Signed document {<field>: "<json>", signature: "<base64>"}; the signature covers
// UTF-8(context) || UTF-8(<field> string), so no canonicalization is involved.
async function signed(bytes, field, maxText, context, publicKey) {
  let doc;
  try { doc = JSON.parse(new TextDecoder("utf-8", {fatal: true}).decode(bytes)); } catch { return {status: 400}; }
  if (!exactKeys(doc, [field, "signature"]) || typeof doc[field] !== "string" || enc.encode(doc[field]).length > maxText) return {status: 400};
  const signature = base64(doc.signature, 64);
  if (!signature) return {status: 400};
  const key = await crypto.subtle.importKey("raw", base64(publicKey, 32), {name: "Ed25519"}, false, ["verify"]);
  if (!await crypto.subtle.verify({name: "Ed25519"}, key, signature, enc.encode(context + doc[field]))) return {status: 403};
  try { return {doc, value: JSON.parse(doc[field])}; } catch { return {status: 400}; }
}

// Ed25519 public key check in BigInt: canonical encoding of a curve point that is not
// the identity and lies in the prime-order subgroup. workerd's verify accepts
// small-order keys (the identity key verifies anything), so this is not optional.
const P = 2n ** 255n - 19n, L = 2n ** 252n + 27742317777372353535851937790883648493n;
const mod = a => ((a % P) + P) % P;
function pow(b, e) {
  let r = 1n;
  for (b = mod(b); e > 0n; e >>= 1n, b = b * b % P) if (e & 1n) r = r * b % P;
  return r;
}
const D = mod(-121665n * pow(121666n, P - 2n)), SQRT_M1 = pow(2n, (P - 1n) / 4n);
function add([X1, Y1, Z1, T1], [X2, Y2, Z2, T2]) {
  const A = mod((Y1 - X1) * (Y2 - X2)), B = mod((Y1 + X1) * (Y2 + X2)), C = mod(2n * D * T1 * T2), Dz = mod(2n * Z1 * Z2);
  const E = B - A, F = Dz - C, G = Dz + C, H = B + A;
  return [mod(E * F), mod(G * H), mod(F * G), mod(E * H)];
}
export function primeOrderPoint(bytes) {
  let y = 0n;
  for (let i = 31; i >= 0; i--) y = (y << 8n) | BigInt(i === 31 ? bytes[i] & 0x7f : bytes[i]);
  const sign = bytes[31] >> 7;
  if (y >= P) return false;
  const u = mod(y * y - 1n), v = mod(D * y * y + 1n);
  let x = mod(u * pow(v, 3n) * pow(u * pow(v, 7n), (P - 5n) / 8n));
  if (mod(v * x * x) !== u) {
    if (mod(v * x * x) !== mod(-u)) return false;
    x = x * SQRT_M1 % P;
  }
  if (x === 0n && sign) return false;
  if (Number(x & 1n) !== sign) x = P - x;
  if (x === 0n && y === 1n) return false; // identity
  let acc = [0n, 1n, 1n, 0n], pt = [x, y, 1n, x * y % P];
  for (let e = L; e > 0n; e >>= 1n, pt = add(pt, pt)) if (e & 1n) acc = add(acc, pt);
  return acc[0] === 0n && acc[1] === acc[2]; // L·P is the identity
}
const keyChecks = new Map(); // Per-isolate memo; only bootstrap and signed rotations reach it.
function publicKeyOk(text) {
  if (!keyChecks.has(text)) {
    if (keyChecks.size >= 16) keyChecks.clear();
    const bytes = base64(text, 32);
    keyChecks.set(text, bytes !== null && primeOrderPoint(bytes));
  }
  return keyChecks.get(text);
}

export function validTable(t) {
  if (!exactKeys(t, ["revision", "issuedAt", "routes", "blockedSenders"]) || !isInt(t.revision) || !isInt(t.issuedAt) ||
      !Array.isArray(t.routes) || t.routes.length > limits.routes ||
      !Array.isArray(t.blockedSenders) || t.blockedSenders.length > limits.blocks) return false;
  const seen = new Set();
  for (const r of t.routes) {
    if (!exactKeys(r, ["address", "generation", "maxBytes"]) || !addressOk(r.address) || seen.has(r.address) ||
        !isInt(r.generation) || !isInt(r.maxBytes) || r.maxBytes > limits.messageBytes) return false;
    seen.add(r.address);
  }
  return t.blockedSenders.every(b => (b?.until === null || isInt(b?.until)) &&
    (exactKeys(b, ["address", "until"]) && addressOk(b.address) || exactKeys(b, ["domain", "until"]) && domainOk(b.domain)));
}

function validCredentials(c) {
  return exactKeys(c, ["epoch", "tokenSha256", "publicKey"]) && isInt(c.epoch) &&
    typeof c.tokenSha256 === "string" && HEX64.test(c.tokenSha256) && publicKeyOk(c.publicKey);
}

// The only read before authentication. Deployed secrets bootstrap epoch 1 only
// while credentials.json is absent.
async function credentials(env) {
  const object = await env.MAIL.get(CREDENTIALS);
  const c = object ? JSON.parse(await object.text())
    : {epoch: 1, tokenSha256: env.PICKUP_TOKEN_SHA256, publicKey: env.ROUTING_PUBLIC_KEY};
  if (!validCredentials(c)) throw new Error("invalid credentials");
  return {...c, etag: object?.httpEtag ?? null};
}

const bearerOf = request => /^Bearer [0-9a-f]{64}$/.exec(request.headers.get("Authorization") ?? "")?.[0].slice(7);

async function bearerOk(bearer, tokenSha256) {
  const got = await sha256(enc.encode(bearer));
  let diff = 0;
  for (let i = 0; i < 32; i++) diff |= got[i] ^ parseInt(tokenSha256.slice(i * 2, i * 2 + 2), 16);
  return diff === 0;
}

const replaceIf = etag => new Headers(etag ? {"If-Match": etag} : {"If-None-Match": "*"});

async function putRoutes(request, env, creds) {
  const bytes = await readBounded(request.body, 2 * limits.tableBytes + 4096);
  if (!bytes) return response(413);
  const {status, doc, value: table} = await signed(bytes, "table", limits.tableBytes, contexts.routes, creds.publicKey);
  if (status) return response(status);
  if (!validTable(table)) return response(400);
  const now = Date.now();
  if (table.revision > now + limits.futureMs || table.issuedAt > now + limits.futureMs ||
      now - table.issuedAt > limits.maxAgeMs) return response(422);
  const current = await env.MAIL.head(ROUTES);
  if (current && !(table.revision > Number(current.customMetadata?.revision))) return response(409);
  const stored = await env.MAIL.put(ROUTES, JSON.stringify({table: doc.table, signature: doc.signature}), {
    onlyIf: replaceIf(current?.httpEtag), customMetadata: {revision: String(table.revision)},
  });
  return response(stored ? 204 : 409);
}

async function rotate(request, env, creds) {
  const bytes = await readBounded(request.body, limits.rotateBody);
  if (!bytes) return response(413);
  const {status, value: next} = await signed(bytes, "rotation", limits.rotateBody, contexts.rotate, creds.publicKey);
  if (status) return response(status);
  if (!validCredentials(next)) return response(400);
  if (next.epoch !== creds.epoch + 1) return response(409);
  // Both secrets must change, or rotation would not fence a holder of the old one.
  if (next.tokenSha256 === creds.tokenSha256 || next.publicKey === creds.publicKey) return response(400);
  // Conditional on the record the bearer was just checked against: a concurrent
  // rotation, or one landing after authentication, loses here.
  const stored = await env.MAIL.put(CREDENTIALS, JSON.stringify(next), {onlyIf: replaceIf(creds.etag)});
  return response(stored ? 204 : 409);
}

function query(url, allowed) {
  const keys = [...url.searchParams.keys()];
  return keys.length === new Set(keys).size && keys.every(k => allowed.includes(k));
}

const envelopeOf = object => JSON.parse(object.customMetadata.envelope);

async function listMessages(url, env) {
  if (!query(url, ["after", "limit"])) return response(404);
  const after = url.searchParams.get("after"), limitText = url.searchParams.get("limit") ?? String(limits.page);
  if (after !== null && !UUID.test(after) || !/^[1-9][0-9]{0,2}$/.test(limitText) || Number(limitText) > limits.page) return response(400);
  const options = {prefix: INBOX, limit: Number(limitText), include: ["customMetadata"], ...(after && {startAfter: INBOX + after})};
  let listed = await env.MAIL.list(options);
  // R2 may return an empty truncated page when metadata is included; follow its cursor.
  while (listed.objects.length === 0 && listed.truncated) listed = await env.MAIL.list({...options, cursor: listed.cursor});
  const messages = listed.objects.map(o => ({key: o.key.slice(INBOX.length), size: o.size, digest: envelopeOf(o).digest}));
  return response(200, JSON.stringify({messages, truncated: listed.truncated}), {"Content-Type": "application/json"});
}

async function route(request, env, url, creds) {
  const {pathname: path, search} = url, method = request.method;
  if (path === "/messages" && method === "GET") return listMessages(url, env);
  const id = path.startsWith("/messages/") && path.slice("/messages/".length);
  if (id && UUID.test(id)) {
    if (method === "GET" && !search) {
      const object = await env.MAIL.get(INBOX + id);
      if (!object) return response(404);
      return response(200, object.body, {
        "Content-Type": "application/octet-stream", "Content-Length": String(object.size),
        "X-KyPost-Envelope": base64url(enc.encode(object.customMetadata.envelope)),
      });
    }
    if (method === "DELETE" && query(url, ["digest"]) && HEX64.test(url.searchParams.get("digest") ?? "")) {
      const object = await env.MAIL.head(INBOX + id);
      if (!object) return response(204);
      if (envelopeOf(object).digest !== url.searchParams.get("digest")) return response(409);
      await env.MAIL.delete(INBOX + id);
      return response(204);
    }
    return response(404);
  }
  if (path === "/routes" && !search) {
    if (method === "GET") {
      const object = await env.MAIL.get(ROUTES);
      return object ? response(200, object.body, {"Content-Type": "application/json"}) : response(404);
    }
    if (method === "PUT") return putRoutes(request, env, creds);
  }
  if (path === "/rotate" && !search && method === "POST") return rotate(request, env, creds);
  return response(404);
}

// Per-isolate parsed table, revalidated against the stored etag on every message.
let cachedTable = null;
async function currentTable(env) {
  const object = await env.MAIL.get(ROUTES, cachedTable ? {onlyIf: new Headers({"If-None-Match": cachedTable.etag})} : {});
  if (!object) { cachedTable = null; return null; }
  if (!("body" in object)) return cachedTable.table; // Not modified.
  const table = JSON.parse(JSON.parse(await object.text()).table);
  cachedTable = {etag: object.httpEtag, table};
  return table;
}

function blocked(table, sender, now) {
  const domain = sender.slice(sender.lastIndexOf("@") + 1);
  return table.blockedSenders.some(b => (b.until === null || b.until > now) &&
    (b.address !== undefined ? b.address === sender.toLowerCase() : b.domain === domain));
}

export default {
  async email(message, env) {
    const recipient = typeof message.to === "string" ? message.to.toLowerCase() : "";
    const from = message.from;
    const at = typeof from === "string" ? from.lastIndexOf("@") : -1;
    // Null sender ("" or "<>") is "". The local part keeps its case; the domain is lowercased ASCII.
    const sender = from === "" || from === "<>" ? "" : at > 0 ? from.slice(0, at + 1) + from.slice(at + 1).toLowerCase() : null;
    if (!addressOk(recipient) || sender === null || sender.length > 512 ||
        sender !== "" && !(/^[^\s<>@\x00-\x1f\x7f]+@[^@]+$/.test(sender) && domainOk(sender.slice(sender.lastIndexOf("@") + 1)))) {
      message.setReject("Address refused");
      return;
    }
    const table = await currentTable(env); // A storage error throws: temporary failure.
    if (!table) { message.setReject("Address refused"); return; }
    const now = Date.now();
    if (sender && blocked(table, sender, now)) { message.setReject("Sender blocked"); return; }
    if (now - table.issuedAt > limits.maxAgeMs) { message.setReject("Routing unavailable"); return; }
    const route = table.routes.find(r => r.address === recipient);
    if (!route) { message.setReject("Address refused"); return; }
    if (!(message.rawSize <= route.maxBytes)) { message.setReject("Message too large"); return; }
    const raw = await readBounded(message.raw, route.maxBytes);
    if (!raw || raw.byteLength === 0) { message.setReject(raw ? "Empty message" : "Message too large"); return; }
    const id = uuidv7(now), digest = hex(await sha256(raw));
    const envelope = JSON.stringify({id, sender, recipient, generation: route.generation,
      tableRevision: table.revision, capturedAt: now, size: raw.byteLength, digest});
    if (enc.encode(envelope).length > limits.envelopeBytes) { message.setReject("Address refused"); return; }
    const stored = await env.MAIL.put(INBOX + id, raw, {
      onlyIf: new Headers({"If-None-Match": "*"}), sha256: digest, customMetadata: {envelope},
    });
    // Never overwrite; a collision is a temporary failure, not acceptance.
    if (!stored) throw new Error("capture key collision");
  },

  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.protocol !== "https:") return response(403);
    const bearer = bearerOf(request); // Malformed headers cost no storage read.
    if (!bearer) return response(401);
    let creds;
    try { creds = await credentials(env); } catch { return response(503); }
    if (!await bearerOk(bearer, creds.tokenSha256)) return response(401);
    try { return await route(request, env, url, creds); } catch { return response(503); }
  },
};
