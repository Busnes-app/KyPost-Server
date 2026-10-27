// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { createECDH } from "node:crypto";
import * as pgp from "openpgp";
import { writeFileSync } from "node:fs";
import { Aes256Gcm, CipherSuite, DhkemP256HkdfSha256, HkdfSha256 } from "@hpke/core";
import cfrg from "../../../testdata/hpke-cfrg-p256-aes256.json";
import fixture from "../../../testdata/device-envelope-hpke.json";
import browser from "../../../testdata/device-envelope-hpke-browser.json";
import { parseKeyring } from "./pgpKeyring";
import { bucketFor, deriveEnrollmentCode, deviceEnvelopeVersion, sealEnvelopeForDevice, sealKeyringForDevice } from "./deviceEnrollment";

const BROWSER_FIXTURE = new URL("../../../testdata/device-envelope-hpke-browser.json", import.meta.url);
const ALG = "HPKE-Base-P256-SHA256-A256GCM";
const suite = new CipherSuite({ kem: new DhkemP256HkdfSha256(), kdf: new HkdfSha256(), aead: new Aes256Gcm() });
const b = (s: string) => Buffer.from(s, "base64");
const hex = (s: string) => Buffer.from(s, "hex");
type Envelope = { v: number; alg: string; enc: string; ct: string };

// Independent info: no production AAD helper is reused.
function info(v: number, deviceId: string, fingerprint: string) {
  const parts = [Buffer.from(`kypost-device-envelope/v${v}`)];
  for (const value of [deviceId, fingerprint]) {
    const field = Buffer.from(value); const size = Buffer.alloc(2); size.writeUInt16BE(field.length);
    parts.push(size, field);
  }
  return Buffer.concat(parts);
}

// Test-only opener: framing, then HPKE with info from the opener's own identity.
async function open(envelope: Envelope, version: number, deviceId: string, fingerprint: string) {
  const enc = b(envelope.enc), ct = b(envelope.ct);
  if (envelope.v !== version || envelope.alg !== ALG || enc.length !== 65 || enc[0] !== 4 || ct.length < 16) throw new Error("framing");
  const recipientKey = await suite.kem.deserializePrivateKey(b(fixture.devicePrivateKey));
  return Buffer.from(await suite.open({ recipientKey, enc, info: info(version, deviceId, fingerprint) }, ct)).toString("utf8");
}

async function keyringInput(vector: { deviceId: string; plaintext: string }) {
  const ring = await parseKeyring(vector.plaintext);
  if (ring.kind !== "keyring") throw new Error("test needs a keyring");
  return {
    raw: vector.plaintext, deviceId: vector.deviceId, publicKeyB64: fixture.devicePublicKey,
    snapshot: { fingerprint: ring.activeKey.getFingerprint().toUpperCase(), publicKey: ring.activeKey.toPublic().armor(), keyring: ring.metadata },
    typedCode: await deriveEnrollmentCode(fixture.devicePublicKey, vector.deviceId, bucketFor(Math.floor(Date.now() / 1000))),
  };
}

// Pins the HPKE ephemeral key, the only randomness in a seal.
async function fixedEphemeral() {
  const ecdh = createECDH("prime256v1");
  ecdh.setPrivateKey(b(browser.ephemeralPrivateKey));
  const pub = ecdh.getPublicKey();
  const privateKey = await crypto.subtle.importKey("jwk", {
    kty: "EC", crv: "P-256", d: b(browser.ephemeralPrivateKey).toString("base64url"),
    x: pub.subarray(1, 33).toString("base64url"), y: pub.subarray(33).toString("base64url"),
  }, { name: "ECDH", namedCurve: "P-256" }, true, ["deriveBits"]);
  const publicKey = await crypto.subtle.importKey("raw", pub, { name: "ECDH", namedCurve: "P-256" }, true, []);
  vi.spyOn(crypto.subtle, "generateKey").mockResolvedValue({ privateKey, publicKey });
}

async function sealBoth() {
  const [v4, v5] = fixture.vectors;
  return [
    await sealEnvelopeForDevice(fixture.devicePublicKey, v4.deviceId, v4.fingerprint, v4.plaintext),
    await sealKeyringForDevice(await keyringInput(v5)),
  ];
}

afterEach(() => vi.restoreAllMocks());

describe("HPKE device envelopes (v4/v5)", () => {
  it("@hpke/core reproduces the CFRG P-256/SHA-256/AES-256-GCM vector", async () => {
    const v = cfrg.vector, first = v.encryptions[0];
    const recipientPublicKey = await suite.kem.deserializePublicKey(hex(v.pkRm));
    const sealed = await suite.seal({ recipientPublicKey, info: hex(v.info), ekm: hex(v.ikmE) }, hex(first.pt), hex(first.aad));
    expect(Buffer.from(sealed.enc).toString("hex")).toBe(v.enc);
    expect(Buffer.from(sealed.ct).toString("hex")).toBe(first.ct);
    const recipientKey = await suite.kem.deserializePrivateKey(hex(v.skRm));
    const pt = await suite.open({ recipientKey, enc: hex(v.enc), info: hex(v.info) }, hex(first.ct), hex(first.aad));
    expect(Buffer.from(pt).toString("hex")).toBe(first.pt);
  });

  it("opens the Go-sealed vectors and refuses every negative at its stage", async () => {
    for (const v of fixture.vectors) {
      expect(Buffer.from(info(v.envelope.v, v.deviceId, v.fingerprint)).toString("base64")).toBe(v.info);
      expect(await open(v.envelope, v.envelope.v, v.deviceId, v.fingerprint)).toBe(v.plaintext);
    }
    expect(fixture.negative).toHaveLength(5);
    for (const n of fixture.negative) {
      const err = await open(n.envelope, n.envelope.v, n.deviceId, n.fingerprint).then(() => null, (e: unknown) => e);
      expect(err, n.name).toBeInstanceOf(Error);
      expect((err as Error).message === "framing", n.name).toBe(n.rejectedBy === "framing");
    }
  });

  it("seals byte-for-byte the browser vectors Go opens", async () => {
    await fixedEphemeral();
    const sealed = await sealBoth();
    if (process.env.KYPOST_WRITE_HPKE_BROWSER_FIXTURE === "1") {
      writeFileSync(BROWSER_FIXTURE, JSON.stringify({ ...browser, vectors: sealed.map((envelope, i) => ({
        deviceId: fixture.vectors[i].deviceId, fingerprint: fixture.vectors[i].fingerprint, envelope,
      })) }, null, 2) + "\n");
    }
    expect(sealed).toEqual(browser.vectors.map(v => v.envelope));
  });

  it("seals with fresh ephemeral keys that open only for the bound device, fingerprint and version", async () => {
    const [v4, v5] = fixture.vectors;
    const [a, c] = await sealBoth(), [b2] = await sealBoth();
    expect(a).toMatchObject({ v: 4, alg: ALG });
    expect(c).toMatchObject({ v: 5, alg: ALG });
    expect((a as Envelope).enc).not.toBe((b2 as Envelope).enc);
    expect(await open(a as Envelope, 4, v4.deviceId, v4.fingerprint)).toBe(v4.plaintext);
    expect(await open(c as Envelope, 5, v5.deviceId, v5.fingerprint)).toBe(v5.plaintext);
    await expect(open(a as Envelope, 4, "other", v4.fingerprint)).rejects.toThrow();
    await expect(open(a as Envelope, 4, v4.deviceId, "0".repeat(40))).rejects.toThrow();
    await expect(open({ ...(a as Envelope), v: 5 }, 5, v4.deviceId, v4.fingerprint)).rejects.toThrow();
  });

  it("refuses a substituted key before generating an ephemeral", async () => {
    const generate = vi.spyOn(crypto.subtle, "generateKey");
    const input = await keyringInput(fixture.vectors[1]);
    await expect(sealKeyringForDevice({ ...input, publicKeyB64: fixture.vectors[1].envelope.enc })).rejects.toThrow(/code/);
    await expect(sealKeyringForDevice({ ...input, typedCode: await deriveEnrollmentCode(input.publicKeyB64, input.deviceId, bucketFor(Math.floor(Date.now() / 1000)) - 2) })).rejects.toThrow(/code/);
    for (const deviceId of ["", "試".repeat(22000)]) await expect(sealKeyringForDevice({ ...input, deviceId })).rejects.toThrow(/device ID/);
    expect(generate).not.toHaveBeenCalled();
  });

  it("seals the original full ring, which opens historical mail", async () => {
    const v5 = fixture.vectors[1];
    const ring = await parseKeyring(await open(await sealKeyringForDevice(await keyringInput(v5)) as Envelope, 5, v5.deviceId, v5.fingerprint));
    expect(ring.keys).toHaveLength(2);
    const message = await pgp.encrypt({ message: await pgp.createMessage({ text: "retained history" }), encryptionKeys: ring.keys[1].toPublic() });
    expect((await pgp.decrypt({ message: await pgp.readMessage({ armoredMessage: message }), decryptionKeys: ring.keys })).data).toBe("retained history");
  });

  it("refuses incomplete, stale, single-key or oversized ring material", async () => {
    const input = await keyringInput(fixture.vectors[1]);
    const ring = await parseKeyring(input.raw);
    for (const material of [fixture.vectors[0].plaintext, input.raw.replace(ring.keys[1].getFingerprint().toUpperCase(), "0".repeat(40)), input.raw + " ".repeat(128 << 10)]) {
      await expect(sealKeyringForDevice({ ...input, raw: material })).rejects.toThrow(/keyring is invalid or unsupported/);
    }
    await expect(sealKeyringForDevice({ ...input, snapshot: { ...input.snapshot, keyring: { ...input.snapshot.keyring, materialGeneration: 3 } } })).rejects.toThrow(/complete keyring/);
    // The parser accepts up to 128 KiB; base64 expansion makes storage admission tighter.
    await expect(sealKeyringForDevice({ ...input, raw: input.raw.padEnd(100 << 10) })).rejects.toThrow(/128 KiB/);
  });

  // v2/v3 are retired: a device that advertises only them gets nothing sealed.
  it("picks HPKE when advertised and never falls back to v2/v3", () => {
    const cases: [boolean, number[], number | null][] = [
      [true, [2, 3, 4, 5], 5], [true, [3, 5], 5], [true, [5], 5], [true, [2, 3], null], [true, [3], null], [true, [2, 4], null], [true, [], null],
      [false, [2, 3, 4, 5], 4], [false, [2, 4], 4], [false, [4], 4], [false, [2, 3], null], [false, [2], null], [false, [3, 5], null], [false, [], null],
    ];
    for (const [converted, advertised, want] of cases) expect(deviceEnvelopeVersion(converted, advertised), `${converted} ${advertised}`).toBe(want);
  });
});
