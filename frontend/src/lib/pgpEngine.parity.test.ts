// @vitest-environment node
// Engine parity and interop: openpgp.js 6.3.3 and the gopenpgp wasm engine
// (scripts/build-pgp-wasm.sh). The whole pgpClient suite also runs on the
// wasm engine with VITE_PGP_ENGINE=gopenpgp-wasm (CI does both).
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { pgpEngine, type PgpEngine } from "./pgpEngine";

const built = existsSync(resolve(import.meta.dirname, "../../.pgp-wasm/kypost-pgp.wasm"));
if (!built && process.env.KYPOST_REQUIRE_PGP_WASM) throw new Error("gopenpgp wasm not built: run scripts/build-pgp-wasm.sh");
if (built) await import("../test/pgpWasmSetup");

const MAX = 1 << 20;
const ids = (email: string) => [{ name: "Test", email }];

async function engines(): Promise<[PgpEngine, PgpEngine]> {
  return [await pgpEngine("openpgp.js"), await pgpEngine("gopenpgp-wasm")];
}

describe.runIf(built)("openpgp.js and gopenpgp wasm agree", () => {
  for (const [producer, consumer] of [[0, 1], [1, 0], [0, 0], [1, 1]] as const) {
    it(`v4: engine ${producer} encrypts+signs, engine ${consumer} decrypts+verifies`, async () => {
      const e = await engines();
      const alice = await e[producer].generate(ids("alice@example.test"), "v4");
      const bob = await e[consumer].generate(ids("bob@example.test"), "v4");
      const msg = await e[producer].encrypt({ text: "line one\nline two\n", encryptionKeys: [bob.armoredPublicKey], signingKey: alice.armoredPrivateKey });
      const out = await e[consumer].decrypt({ armoredMessage: msg, decryptionKeys: [bob.armoredPrivateKey], verificationKeys: [alice.armoredPublicKey], maxBytes: MAX });
      expect(out.data).toBe("line one\nline two\n");
      const signer = await e[consumer].inspect(alice.armoredPublicKey);
      expect(out.signatures).toHaveLength(1);
      expect(out.signatures[0].valid).toBe(true);
      expect(signer.keyIDs.includes(out.signatures[0].keyID) || signer.fingerprints.includes(out.signatures[0].fingerprint)).toBe(true);
      expect(signer.fingerprint).toBe(alice.fingerprint);

      const part = new TextEncoder().encode("Content-Type: text/plain\r\n\r\nsigned\r\n");
      const { armoredSignature, hashAlgorithm } = await e[producer].signDetached(part, alice.armoredPrivateKey);
      expect(hashAlgorithm).toBeGreaterThan(0);
      const good = await e[consumer].verifyDetached({ data: part, armoredSignature, verificationKeys: [alice.armoredPublicKey] });
      expect(good.map((s) => s.valid)).toEqual([true]);
      // Mutation: one changed byte fails on every engine.
      const bad = await e[consumer].verifyDetached({ data: part.map((b, i) => (i === part.length - 3 ? b ^ 1 : b)), armoredSignature, verificationKeys: [alice.armoredPublicKey] });
      expect(bad.map((s) => s.valid)).toEqual([false]);
    }, 30000);
  }

  it("wrong recipient: neither engine decrypts with a key the message was not sent to", async () => {
    for (const engine of await engines()) {
      const bob = await engine.generate(ids("bob@example.test"), "v4");
      const eve = await engine.generate(ids("eve@example.test"), "v4");
      const msg = await engine.encrypt({ text: "for bob", encryptionKeys: [bob.armoredPublicKey] });
      await expect(engine.decrypt({ armoredMessage: msg, decryptionKeys: [eve.armoredPrivateKey], verificationKeys: [], maxBytes: MAX })).rejects.toThrow();
    }
  }, 30000);

  it("armor and dearmor round-trip identical bytes across engines", async () => {
    const [js, wasm] = await engines();
    const key = await js.generate(ids("a@example.test"), "v4");
    const raw = await js.dearmor(key.armoredPublicKey);
    expect(await wasm.dearmor(key.armoredPublicKey)).toEqual(raw);
    expect(await js.dearmor(await wasm.armor(raw, "PGP PUBLIC KEY BLOCK"))).toEqual(raw);
    expect(await wasm.dearmor(await js.armor(raw, "PGP PUBLIC KEY BLOCK"))).toEqual(raw);
  }, 30000);

  it("v6 PQC: wasm generates ML-DSA-65+Ed25519 / ML-KEM-768+X25519 and round-trips; openpgp.js 6.3 cannot read it", async () => {
    const [js, wasm] = await engines();
    const alice = await wasm.generate(ids("alice@example.test"), "v6");
    const bob = await wasm.generate(ids("bob@example.test"), "v6");
    const info = await wasm.inspect(bob.armoredPublicKey);
    expect(info.version).toBe(6);
    expect(info.algorithms).toEqual([30, 35]);
    expect(info.fingerprint).toMatch(/^[0-9A-F]{64}$/);
    const msg = await wasm.encrypt({ text: "post-quantum", encryptionKeys: [bob.armoredPublicKey], signingKey: alice.armoredPrivateKey });
    const out = await wasm.decrypt({ armoredMessage: msg, decryptionKeys: [bob.armoredPrivateKey], verificationKeys: [alice.armoredPublicKey], maxBytes: MAX });
    expect(out.data).toBe("post-quantum");
    expect(out.signatures.map((s) => s.valid)).toEqual([true]);
    await expect(wasm.decrypt({ armoredMessage: msg, decryptionKeys: [alice.armoredPrivateKey], verificationKeys: [], maxBytes: MAX })).rejects.toThrow();
    // One delivery group never mixes v4 and v6 (spec A, "Mixed recipients").
    const v4 = await wasm.generate(ids("c@example.test"), "v4");
    await expect(wasm.encrypt({ text: "x", encryptionKeys: [bob.armoredPublicKey, v4.armoredPublicKey] })).rejects.toThrow(/mix/);
    await expect(js.inspect(bob.armoredPublicKey)).rejects.toThrow();
  }, 30000);
});

// The P0 shared fixtures (testdata/openpgp-v6, bracken's feat/pqc-p0-key-policy).
// Point KYPOST_PGP_FIXTURES at that directory to run; not in this branch yet.
const fixtures = process.env.KYPOST_PGP_FIXTURES;
describe.runIf(built && Boolean(fixtures))("P0 fixtures", () => {
  const read = (name: string) => readFileSync(`${fixtures}/${name}`, "utf8");
  for (const name of ["v4", "v4-pqc", "v6-pqc"]) {
    it(`wasm opens ${name}.msg.asc`, async () => {
      const wasm = await pgpEngine("gopenpgp-wasm");
      const out = await wasm.decrypt({ armoredMessage: read(`${name}.msg.asc`), decryptionKeys: [read(`${name}.sec.asc`)], verificationKeys: [read(`${name}.pub.asc`)], maxBytes: MAX });
      expect(out.data.replace(/\r\n/g, "\n")).toBe(read("message.txt").replace(/\r\n/g, "\n"));
    });
  }
  it("wasm verifies v6-pqc.sig.asc over message.txt", async () => {
    const wasm = await pgpEngine("gopenpgp-wasm");
    const checks = await wasm.verifyDetached({ data: readFileSync(`${fixtures}/message.txt`), armoredSignature: read("v6-pqc.sig.asc"), verificationKeys: [read("v6-pqc.pub.asc")] });
    expect(checks.map((s) => s.valid)).toEqual([true]);
  });
  it("openpgp.js 6.3 opens v4.msg.asc and refuses the PQC ones", async () => {
    const js = await pgpEngine("openpgp.js");
    const out = await js.decrypt({ armoredMessage: read("v4.msg.asc"), decryptionKeys: [read("v4.sec.asc")], verificationKeys: [], maxBytes: MAX });
    expect(out.data.replace(/\r\n/g, "\n")).toBe(read("message.txt").replace(/\r\n/g, "\n"));
    await expect(js.decrypt({ armoredMessage: read("v6-pqc.msg.asc"), decryptionKeys: [read("v6-pqc.sec.asc")], verificationKeys: [], maxBytes: MAX })).rejects.toThrow();
  });
});
