// Runs inside Chromium. Loaded by bench.mjs; reports one JSON result.
const enc = new TextEncoder();
const median = (xs) => xs.slice().sort((a, b) => a - b)[Math.floor(xs.length / 2)];
async function time(n, fn) {
  const xs = [];
  for (let i = 0; i < n; i++) { const t = performance.now(); await fn(); xs.push(performance.now() - t); }
  return { median: +median(xs).toFixed(2), max: +Math.max(...xs).toFixed(2) };
}
const body = "Content-Type: text/plain\r\n\r\n" + "x".repeat(16 * 1024) + "\r\n";

export async function run(engine, n) {
  const out = { engine };
  const t0 = performance.now();
  let api, mem;
  if (engine === "wasm") {
    await import("/wasm_exec.js");
    const go = new Go();
    const { instance } = await WebAssembly.instantiateStreaming(fetch("/kypost-pgp.wasm"), go.importObject);
    out.instantiateMs = +(performance.now() - t0).toFixed(1);
    go.run(instance);
    api = globalThis.kypostPGP;
    mem = () => instance.exports.mem.buffer.byteLength;
  } else {
    api = await import("/openpgp.min.mjs");
    out.instantiateMs = +(performance.now() - t0).toFixed(1);
  }
  out.loadMs = +(performance.now() - t0).toFixed(1);
  const w = (op, req) => { const r = api[op](req); if (r.error) throw new Error(r.error); return r; };

  for (const profile of engine === "wasm" ? ["v4", "v6"] : ["v4"]) {
    const r = {};
    let a, b;
    if (engine === "wasm") {
      r.generate = await time(n, () => { a = w("generate", { profile, userIDs: [{ name: "A", email: "a@example.test" }] }); });
      b = w("generate", { profile, userIDs: [{ name: "B", email: "b@example.test" }] });
      r.publicKeyBytes = a.publicKey.length;
      let msg;
      r.encryptSign = await time(n, () => { msg = w("encrypt", { data: enc.encode(body), encryptionKeys: [enc.encode(b.publicKey)], signingKey: enc.encode(a.privateKey) }).message; });
      r.messageBytes = msg.length;
      r.decryptVerify = await time(n, () => { w("decrypt", { message: enc.encode(msg), decryptionKeys: [enc.encode(b.privateKey)], verificationKeys: [enc.encode(a.publicKey)], maxBytes: 1 << 25 }); });
      let sig;
      r.signDetached = await time(n, () => { sig = w("signDetached", { data: enc.encode(body), signingKey: enc.encode(a.privateKey) }).signature; });
      r.verifyDetached = await time(n, () => { w("verifyDetached", { data: enc.encode(body), signature: enc.encode(sig), verificationKeys: [enc.encode(a.publicKey)] }); });
      r.wasmMemoryBytes = mem();
    } else {
      const o = api;
      const gen = (email) => o.generateKey({ type: "ecc", curve: "curve25519Legacy", userIDs: [{ name: "A", email }], format: "armored" });
      r.generate = await time(n, async () => { a = await gen("a@example.test"); });
      b = await gen("b@example.test");
      r.publicKeyBytes = a.publicKey.length;
      const pubB = await o.readKey({ armoredKey: b.publicKey }), privA = await o.readPrivateKey({ armoredKey: a.privateKey });
      const privB = await o.readPrivateKey({ armoredKey: b.privateKey }), pubA = await o.readKey({ armoredKey: a.publicKey });
      let msg;
      r.encryptSign = await time(n, async () => { msg = await o.encrypt({ message: await o.createMessage({ text: body }), encryptionKeys: pubB, signingKeys: privA }); });
      r.messageBytes = msg.length;
      r.decryptVerify = await time(n, async () => {
        const res = await o.decrypt({ message: await o.readMessage({ armoredMessage: msg }), decryptionKeys: privB, verificationKeys: pubA });
        await res.signatures[0].verified;
      });
      let sig;
      r.signDetached = await time(n, async () => { sig = await o.sign({ message: await o.createMessage({ binary: enc.encode(body) }), signingKeys: privA, detached: true }); });
      r.verifyDetached = await time(n, async () => {
        const res = await o.verify({ message: await o.createMessage({ binary: enc.encode(body) }), signature: await o.readSignature({ armoredSignature: sig }), verificationKeys: pubA });
        await res.signatures[0].verified;
      });
    }
    out[profile] = r;
  }
  return out;
}
