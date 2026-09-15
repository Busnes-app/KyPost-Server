// @vitest-environment node
import { afterEach, describe, expect, it, vi } from "vitest";
import { createDecipheriv, createECDH, hkdfSync } from "node:crypto";
import * as pgp from "openpgp";
import fixture from "../../../testdata/device-envelope-v3.json";
import { bucketFor, deriveEnrollmentCode, sealEnvelopeForDevice, sealKeyringForDevice } from "./deviceEnrollment";
import { parseKeyring } from "./pgpKeyring";

const vector = fixture.vectors[1];
const raw = vector.plaintext;
const b = (s: string) => Buffer.from(s, "base64");
async function snapshotFor() {
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring") throw new Error("test needs a keyring");
  return { fingerprint: ring.activeKey.getFingerprint().toUpperCase(), publicKey: ring.activeKey.toPublic().armor(), keyring: ring.metadata };
}
async function inputFor() {
  return { raw, snapshot: await snapshotFor(), deviceId: vector.deviceId,
    publicKeyB64: fixture.devicePublicKey,
    typedCode: await deriveEnrollmentCode(fixture.devicePublicKey, vector.deviceId, bucketFor(Math.floor(Date.now() / 1000))) };
}
// Independent Node crypto opener: no production AAD/KDF helper is reused.
function open(envelope: { v: number; epk: string; iv: string; ct: string }, deviceId = vector.deviceId, fingerprint = vector.fingerprint, domainVersion = envelope.v, expected?: typeof vector) {
  const device = createECDH("prime256v1");
  device.setPrivateKey(b(fixture.devicePrivateKey));
  const domain = `kypost-device-envelope/v${domainVersion}`;
  const shared = device.computeSecret(b(envelope.epk));
  const key = hkdfSync("sha256", shared, device.getPublicKey(), domain, 32);
  const parts = [Buffer.from(domain)];
  for (const value of [deviceId, fingerprint]) {
    const field = Buffer.from(value); const size = Buffer.alloc(2); size.writeUInt16BE(field.length);
    parts.push(size, field);
  }
  const decipher = createDecipheriv("aes-256-gcm", key, b(envelope.iv));
  const aad = Buffer.concat(parts);
  if (expected) {
    expect(shared.toString("base64")).toBe(expected.sharedSecret);
    expect(Buffer.from(key).toString("base64")).toBe(expected.aesKey);
    expect(aad.toString("base64")).toBe(expected.aad);
  }
  decipher.setAAD(aad);
  const ct = b(envelope.ct); decipher.setAuthTag(ct.subarray(-16));
  return Buffer.concat([decipher.update(ct.subarray(0, -16)), decipher.final()]).toString("utf8");
}
async function fixedRandomness() {
  const epk = b(vector.envelope.epk);
  const privateKey = await crypto.subtle.importKey("jwk", {
    kty: "EC", crv: "P-256", d: b(fixture.ephemeralPrivateKey).toString("base64url"),
    x: epk.subarray(1, 33).toString("base64url"), y: epk.subarray(33).toString("base64url"),
  }, { name: "ECDH", namedCurve: "P-256" }, true, ["deriveBits"]);
  const publicKey = await crypto.subtle.importKey("raw", epk, { name: "ECDH", namedCurve: "P-256" }, true, []);
  vi.spyOn(crypto.subtle, "generateKey").mockResolvedValue({ privateKey, publicKey });
  vi.spyOn(crypto, "getRandomValues").mockImplementation(array => {
    if (!(array instanceof Uint8Array) || array.length !== 12) throw new Error("unexpected random request");
    array.set(b(vector.envelope.iv)); return array;
  });
}
afterEach(() => vi.restoreAllMocks());

describe("device-envelope v3 preparation", () => {
  it("matches fixed cross-language bytes and leaves v2 byte-compatible", async () => {
    for (const entry of fixture.vectors) {
      expect(open(entry.envelope, entry.deviceId, entry.fingerprint, entry.envelope.v, entry)).toBe(entry.plaintext);
    }
    const input = await inputFor();
    await fixedRandomness();
    expect(await sealKeyringForDevice(input)).toEqual(vector.envelope);
    const legacy = fixture.vectors[0];
    expect(await sealEnvelopeForDevice(fixture.devicePublicKey, legacy.deviceId, legacy.fingerprint, legacy.plaintext)).toEqual(legacy.envelope);
  });

  it("seals original full-ring bytes with fresh entropy and opens historical mail", async () => {
    const input = await inputFor();
    const first = await sealKeyringForDevice(input), second = await sealKeyringForDevice(input);
    expect(first.epk).not.toBe(second.epk); expect(first.iv).not.toBe(second.iv);
    expect(open(first)).toBe(raw); expect(open(second)).toBe(raw);
    const ring = await parseKeyring(open(first));
    expect(ring.keys).toHaveLength(2);
    const message = await pgp.encrypt({ message: await pgp.createMessage({ text: "retained history" }), encryptionKeys: ring.keys[1].toPublic() });
    expect((await pgp.decrypt({ message: await pgp.readMessage({ armoredMessage: message }), decryptionKeys: ring.keys })).data).toBe("retained history");
  });

  it("authenticates device, fingerprint, domain, ciphertext and tag", async () => {
    const envelope = await sealKeyringForDevice(await inputFor());
    expect(() => open(envelope, "different-device")).toThrow();
    expect(() => open(envelope, vector.deviceId, "0".repeat(40))).toThrow();
    expect(() => open(envelope, vector.deviceId, vector.fingerprint, 2)).toThrow();
    for (const index of [0, b(envelope.ct).length - 1]) {
      const ct = b(envelope.ct); ct[index] ^= 1;
      expect(() => open({ ...envelope, ct: ct.toString("base64") })).toThrow();
    }
  });

  it("refuses substituted keys, stale codes and invalid IDs before generating a key", async () => {
    const input = await inputFor();
    const generate = vi.spyOn(crypto.subtle, "generateKey");
    await expect(sealKeyringForDevice({ ...input, publicKeyB64: vector.envelope.epk })).rejects.toThrow(/code/);
    await expect(sealKeyringForDevice({ ...input, typedCode: await deriveEnrollmentCode(input.publicKeyB64, input.deviceId, bucketFor(Math.floor(Date.now() / 1000)) - 2) })).rejects.toThrow(/code/);
    for (const deviceId of ["", "試".repeat(22000)]) await expect(sealKeyringForDevice({ ...input, deviceId })).rejects.toThrow(/device ID/);
    expect(generate).not.toHaveBeenCalled();
  });

  it("refuses incomplete, stale, legacy or oversized material", async () => {
    const input = await inputFor();
    const ring = await parseKeyring(raw);
    for (const material of [fixture.vectors[0].plaintext, raw.replace(ring.keys[1].getFingerprint().toUpperCase(), "0".repeat(40)), raw + " ".repeat(128 << 10)]) {
      await expect(sealKeyringForDevice({ ...input, raw: material })).rejects.toThrow(/keyring is invalid or unsupported/);
    }
    await expect(sealKeyringForDevice({ ...input, snapshot: { ...input.snapshot, keyring: { ...input.snapshot.keyring, materialGeneration: 3 } } })).rejects.toThrow(/complete keyring/);
    // The parser accepts up to 128 KiB; base64 expansion makes storage admission tighter.
    await expect(sealKeyringForDevice({ ...input, raw: raw.padEnd(100 << 10) })).rejects.toThrow(/128 KiB/);
  });
});
