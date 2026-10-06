const slot = "pilot-message";
const maxBytes = 4 << 20;
const hex = bytes => Array.from(new Uint8Array(bytes), b => b.toString(16).padStart(2, "0")).join("");
const boundedText = value => typeof value === "string" && value.length > 0 && value.length <= 512 && !/[\x00-\x1f\x7f]/.test(value);

function routeFrom(env, now) {
  const raw = env.ROUTE;
  if (typeof raw !== "string" || new TextEncoder().encode(raw).length > 4096) throw new Error("Invalid pilot route");
  const route = JSON.parse(raw);
  const fields = ["recipient", "issuer", "subject", "mailbox", "source", "revision", "validUntil"];
  if (!route || Object.keys(route).length !== fields.length || fields.some(f => !(f in route)) ||
      fields.slice(0, 5).some(f => !boundedText(route[f])) ||
      !/^[\x21-\x7e]+@[^@\s]+$/.test(route.recipient) || route.recipient !== route.recipient.toLowerCase() ||
      !Number.isSafeInteger(route.revision) || route.revision <= 0 ||
      !Number.isSafeInteger(route.validUntil) || route.validUntil <= now || route.validUntil - now > 900) throw new Error("Expired or invalid pilot route");
  return route;
}

async function readRaw(stream) {
  const reader = stream.getReader();
  const chunks = [];
  let size = 0;
  try {
    for (;;) {
      const {value, done} = await reader.read();
      if (done) break;
      if (!(value instanceof Uint8Array) || (size += value.byteLength) > maxBytes) {
        await reader.cancel();
        throw new Error("Message exceeds pilot limit");
      }
      chunks.push(value);
    }
  } finally { reader.releaseLock(); }
  if (size === 0) throw new Error("Empty message");
  const raw = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { raw.set(chunk, offset); offset += chunk.byteLength; }
  return raw;
}

async function authorized(request, token) {
  if (typeof token !== "string" || !/^[a-f0-9]{64}$/.test(token)) return false;
  const received = request.headers.get("Authorization");
  if (!received || !/^Bearer [a-f0-9]{64}$/.test(received)) return false;
  const encoder = new TextEncoder();
  const [a, b] = await Promise.all([token, received.slice(7)].map(v => crypto.subtle.digest("SHA-256", encoder.encode(v))));
  const left = new Uint8Array(a), right = new Uint8Array(b);
  let diff = 0;
  for (let i = 0; i < left.length; i++) diff |= left[i] ^ right[i];
  return diff === 0;
}

const response = (status, body = "") => new Response(body, {status, headers: {"Cache-Control": "no-store"}});

export default {
  async email(message, env) {
    let route, raw;
    let now = Math.floor(Date.now() / 1000);
    try {
      route = routeFrom(env, now);
      if (!/^[a-f0-9]{64}$/.test(env.PICKUP_TOKEN ?? "") || message.to !== route.recipient || typeof message.from !== "string" || message.from.length > 512 || message.from !== "" && !/^[^\s<>@]+@[^\s<>@]+$/.test(message.from) || /[\x00-\x1f\x7f]/.test(message.from) || message.rawSize > maxBytes) throw new Error("Pilot envelope refused");
      raw = await readRaw(message.raw);
      now = Math.floor(Date.now() / 1000);
      if (route.validUntil <= now) throw new Error("Capture window closed");
    } catch {
      message.setReject("Pilot route unavailable or message too large");
      return;
    }
    // ponytail: one immutable slot bounds storage and avoids a deletion race.
    // Ongoing reception needs a separately qualified queue/cleanup protocol.
    const digest = hex(await crypto.subtle.digest("SHA-256", raw));
    const envelope = {id: crypto.randomUUID(), sender: message.from, capturedAt: now, size: raw.byteLength, digest, route};
    const metadata = JSON.stringify(envelope);
    if (new TextEncoder().encode(metadata).length > 4096) {
      message.setReject("Pilot metadata exceeds limit");
      return;
    }
    try {
      const object = await env.MAIL.put(slot, raw, {
        onlyIf: new Headers({"If-None-Match": "*"}),
        sha256: digest,
        customMetadata: {envelope: metadata},
      });
      if (!object) message.setReject("Pilot holding slot is occupied");
    } catch {
      // No promise of Cloudflare retry semantics. A failed write must never
      // complete as successful acceptance or expose provider diagnostics.
      message.setReject("Pilot storage unavailable");
    }
  },

  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.protocol !== "https:" || !await authorized(request, env.PICKUP_TOKEN)) return response(401);
    if (request.method !== "GET" || url.pathname !== "/message" || url.search) return response(404);
    try {
      const object = await env.MAIL.get(slot);
      if (!object) return response(404);
      const envelope = object.customMetadata?.envelope;
      if (typeof envelope !== "string" || new TextEncoder().encode(envelope).length > 4096 || object.size <= 0 || object.size > maxBytes) return response(503);
      // UTF-8 metadata encoding leaves MIME bytes entirely untouched.
      const encoded = btoa(String.fromCharCode(...new TextEncoder().encode(envelope)));
      return new Response(object.body, {headers: {
        "Content-Type": "application/octet-stream", "Content-Length": String(object.size),
        "Cache-Control": "no-store", "X-Kypost-Envelope": encoded,
      }});
    } catch { return response(503); }
  },
};
