// The production OpenPGP engine in a real browser: the built dist, served by
// the real server with its real CSP. Loads the engine chunk the app itself
// loads, runs it on openpgp.js-made v4 history and the RFC 9980 fixtures,
// and checks the module is sent precompressed and fails cleanly.
import { test, expect } from '@playwright/test';
import { readdirSync, readFileSync } from 'node:fs';

const assets = new URL('../dist/assets/', import.meta.url);
const chunk = '/assets/' + readdirSync(assets).find((f) => /^pgpEngineWasm-.*\.js$/.test(f));
const fx = (name) => readFileSync(new URL(`../../testdata/openpgp-v6/${name}`, import.meta.url), 'utf8');
const ring = JSON.parse(readFileSync(new URL('../../testdata/pgp-keyring-v1.json', import.meta.url), 'utf8'));

test.beforeEach(({}, info) => test.skip(info.project.name !== 'light-1280', 'engine behaviour does not depend on theme or width'));

async function openApp(page) {
  const violations = [];
  await page.exposeBinding('__csp', (_s, v) => violations.push(v));
  await page.addInitScript(() => document.addEventListener('securitypolicyviolation', (e) => window.__csp(`${e.violatedDirective} ${e.blockedURI}`)));
  const response = await page.goto('/read');
  return { violations, csp: response.headers()['content-security-policy'] };
}

test('the shipped engine runs under the production CSP', async ({ page }) => {
  const { violations, csp } = await openApp(page);
  const scriptSrc = (csp ?? '').split('; ').find((d) => d.startsWith('script-src '))?.split(' ');
  expect(scriptSrc, `document CSP: ${csp}`).toBeTruthy();
  expect(scriptSrc).toContain("'wasm-unsafe-eval'");
  expect(scriptSrc).not.toContain("'unsafe-eval'");

  const wasmResponse = page.waitForResponse((r) => r.url().endsWith('.wasm'));
  const out = await page.evaluate(async ([chunk, fx, ring]) => {
    const t0 = performance.now();
    const { wasmEngine: e } = await import(chunk);
    const alice = await e.generate([{ name: 'A', email: 'a@example.test' }], 'v4');
    const loadMs = performance.now() - t0;
    const bob = await e.generate([{ name: 'B', email: 'b@example.test' }], 'v4');
    const eve = await e.generate([{ name: 'E', email: 'e@example.test' }], 'v4');
    const text = 'Grüße, 日本語 ✓\nline two\n';
    const msg = await e.encrypt({ text, encryptionKeys: [bob.armoredPublicKey], signingKey: alice.armoredPrivateKey });
    const opened = await e.decrypt({ armoredMessage: msg, decryptionKeys: [bob.armoredPrivateKey], verificationKeys: [alice.armoredPublicKey], maxBytes: 1 << 20 });
    const wrongRecipient = await e.decrypt({ armoredMessage: msg, decryptionKeys: [eve.armoredPrivateKey], verificationKeys: [], maxBytes: 1 << 20 }).then(() => 'opened', () => 'refused');
    const data = new TextEncoder().encode('signed part\r\n');
    const sig = await e.signDetached(data, alice.armoredPrivateKey);
    const good = await e.verifyDetached({ data, armoredSignature: sig.armoredSignature, verificationKeys: [alice.armoredPublicKey] });
    const wrongSigner = await e.verifyDetached({ data, armoredSignature: sig.armoredSignature, verificationKeys: [eve.armoredPublicKey] });
    // History openpgp.js wrote: the shared keyring fixture.
    const history = await e.decrypt({ armoredMessage: ring.ciphertext, decryptionKeys: ring.ring.keys.map((k) => k.privateKey), verificationKeys: [], maxBytes: 1 << 20 });
    // RFC 9980 fixtures (go-crypto, cross-checked with Bouncy Castle).
    const pq = {}, pqTamper = {};
    for (const name of ['v4', 'v4-pqc', 'v6-pqc']) {
      pq[name] = (await e.decrypt({ armoredMessage: fx[`${name}.msg.asc`], decryptionKeys: [fx[`${name}.sec.asc`]], verificationKeys: [], maxBytes: 1 << 20 })).data;
      const binary = await e.dearmor(fx[`${name}.msg.asc`]); binary[binary.length - 1] ^= 1;
      pqTamper[name] = await e.decrypt({ armoredMessage: await e.armor(binary, 'PGP MESSAGE'), decryptionKeys: [fx[`${name}.sec.asc`]], verificationKeys: [], maxBytes: 1 << 20 }).then(() => 'opened', () => 'refused');
    }
    const pqSig = await e.verifyDetached({ data: new TextEncoder().encode(fx['message.txt']), armoredSignature: fx['v6-pqc.sig.asc'], verificationKeys: [fx['v6-pqc.pub.asc']] });
    const badSig = await e.verifyDetached({ data: new TextEncoder().encode(fx['message.txt'] + 'changed'), armoredSignature: fx['v6-pqc.sig.asc'], verificationKeys: [fx['v6-pqc.pub.asc']] });
    const classical = await e.encrypt({ text: 'RFC 9580', encryptionKeys: [fx['v6.pub.asc']], signingKey: fx['v6.sec.asc'] });
    const v6Read = await e.decrypt({ armoredMessage: classical, decryptionKeys: [fx['v6.sec.asc']], verificationKeys: [fx['v6.pub.asc']], maxBytes: 1 << 20 });
    const v6 = await e.inspect(fx['v6-pqc.pub.asc']);
    return { loadMs, opened, wrongRecipient, good, wrongSigner, history: history.data, pq, pqTamper, pqSig, badSig, v6Read, v6: { version: v6.version, algorithms: v6.algorithms, fingerprintLength: v6.fingerprint.length },
      heap: performance.memory?.usedJSHeapSize ?? null };
  }, [chunk, Object.fromEntries(['v4.msg.asc', 'v4.sec.asc', 'v4-pqc.msg.asc', 'v4-pqc.sec.asc', 'v6-pqc.msg.asc', 'v6-pqc.sec.asc', 'v6-pqc.sig.asc', 'v6-pqc.pub.asc', 'v6.pub.asc', 'v6.sec.asc', 'message.txt'].map((n) => [n, fx(n)])), ring]);

  const wasm = await wasmResponse;
  expect(wasm.headers()['content-type']).toBe('application/wasm');
  expect(wasm.headers()['content-encoding']).toBe('br');
  expect(out.opened.data).toBe('Grüße, 日本語 ✓\nline two\n');
  expect(out.opened.signatures.map((s) => s.valid)).toEqual([true]);
  expect(out.wrongRecipient).toBe('refused');
  expect(out.good.map((s) => s.valid)).toEqual([true]);
  expect(out.wrongSigner.map((s) => s.valid)).toEqual([false]);
  expect(out.history).toBe(ring.plaintext);
  for (const name of ['v4', 'v4-pqc', 'v6-pqc']) expect(out.pq[name].replace(/\r\n/g, '\n')).toBe(fx('message.txt').replace(/\r\n/g, '\n'));
  expect(out.pqSig.map((s) => s.valid)).toEqual([true]);
  expect(Object.values(out.pqTamper)).toEqual(['refused', 'refused', 'refused']);
  expect(out.badSig.map(s => s.valid)).toEqual([false]);
  expect(out.v6Read.data).toBe('RFC 9580');
  expect(out.v6Read.signatures.map(s => s.valid)).toEqual([true]);
  expect(out.v6).toEqual({ version: 6, algorithms: [30, 35], fingerprintLength: 64 });
  expect(violations).toEqual([]);
  // Evidence for the PR, not a pass condition.
  test.info().annotations.push({ type: 'engine', description: JSON.stringify({ loadMs: Math.round(out.loadMs), heapBytes: out.heap, wasmBytes: (await wasm.body()).length }) });
});

for (const [name, fault] of [['missing', (route) => route.fulfill({ status: 404, body: '' })], ['corrupt', (route) => route.fulfill({ status: 200, contentType: 'application/wasm', body: Buffer.from('not wasm') })]]) {
  test(`a ${name} module fails with a clear error and the next use retries`, async ({ page }) => {
    await openApp(page);
    await page.route('**/*.wasm', fault);
    const first = await page.evaluate(async (chunk) => {
      const { wasmEngine: e } = await import(chunk);
      return e.generate([{ name: 'A', email: 'a@example.test' }], 'v4').then(() => 'ok', (err) => `${err.name}: ${err.message}`);
    }, chunk);
    expect(first).toBe('PgpEngineLoadError: The encryption engine could not load. Check your connection and reload the page.');
    await page.unroute('**/*.wasm');
    const second = await page.evaluate(async (chunk) => {
      const { wasmEngine: e } = await import(chunk);
      return (await e.generate([{ name: 'A', email: 'a@example.test' }], 'v4')).fingerprint.length;
    }, chunk);
    expect(second).toBe(40);
  });
}


test('X-Wing v6 device envelopes run under the app-shell CSP', async ({ page }) => {
  const { violations, csp } = await openApp(page);
  expect(csp).toContain("'wasm-unsafe-eval'");
  const vector = JSON.parse(readFileSync(new URL('../../testdata/device-envelope-xwing.json', import.meta.url), 'utf8'));
  const out = await page.evaluate(async ([chunk, v]) => {
    const { loadBridge } = await import(chunk);
    const bridge = await loadBridge();
    const b = (s) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
    const seed = b(v.seed);
    const fields = [v.deviceId, v.fingerprint].map((s) => new TextEncoder().encode(s));
    const prefix = new TextEncoder().encode('kypost-device-envelope/v6');
    const info = new Uint8Array(prefix.length + fields.reduce((n, f) => n + 2 + f.length, 0));
    info.set(prefix); let at = prefix.length;
    for (const f of fields) { info[at++] = f.length >> 8; info[at++] = f.length & 255; info.set(f, at); at += f.length; }
    try {
      const go = bridge.xwingOpen({ seed, info, enc:b(v.envelope.enc), ct:b(v.envelope.ct) });
      const sealed = bridge.xwingSeal({ publicKey:b(v.publicKey), info, plaintext:new TextEncoder().encode(v.plaintext) });
      const opened = bridge.xwingOpen({ seed, info, enc:sealed.enc, ct:sealed.ct });
      const wrong = info.slice(); wrong[wrong.length - 1] ^= 1;
      const refused = bridge.xwingOpen({ seed, info:wrong, enc:sealed.enc, ct:sealed.ct });
      return { go:new TextDecoder().decode(go.plaintext), opened:new TextDecoder().decode(opened.plaintext), encBytes:sealed.enc.length, refused:typeof refused.error === 'string' };
    } finally { seed.fill(0); }
  }, [chunk, vector]);
  expect(out).toEqual({ go:vector.plaintext, opened:vector.plaintext, encBytes:1120, refused:true });
  expect(violations).toEqual([]);
});
