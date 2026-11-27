// @vitest-environment node
import { readFileSync } from "node:fs";
import { afterEach, beforeAll, expect, it } from "vitest";
import { pgpEngine } from "./pgpEngine";
import { parseKeyring, validateKeyringSnapshot } from "./pgpKeyring";
import { createRecoveryBackup, lock, restoreRecoveryBackup, unlockWithArmoredKey, wrapPrivateKey } from "./keyVault";
import { generateIdentity, openSealedToSelf } from "./pgpClient";
import { bucketFor, deriveEnrollmentCode, openDeviceEnvelope, sealKeyringForDevice } from "./deviceEnrollment";
import device from "../../../testdata/device-envelope-xwing.json";

async function publicOf(privateKey: string) {
  const engine = await pgpEngine();
  return engine.armor((await engine.inspect(privateKey)).publicKey, "PGP PUBLIC KEY BLOCK");
}
const read = (name: string) => readFileSync(new URL(`../../../testdata/openpgp-v6/${name}`, import.meta.url), "utf8");
let v2: { format: string; materialGeneration: number; active: { v4?: string; v6?: string }; keyFingerprints: string[]; keys: { fingerprint: string; privateKey: string }[] };
beforeAll(async () => {
  const engine = await pgpEngine();
  const keys = await Promise.all(["v4", "v6-pqc"].map(async name => {
    const privateKey = read(`${name}.sec.asc`);
    return { privateKey, fingerprint: (await engine.inspect(privateKey)).fingerprint };
  }));
  v2 = { format: "kypost-pgp-keyring-v2", materialGeneration: 2, active: { v4: keys[0].fingerprint, v6: keys[1].fingerprint }, keys,
    keyFingerprints: (await Promise.all(keys.map(async key => (await engine.inspect(key.privateKey)).fingerprints))).flat() };
});
afterEach(lock);

it("reads both active certificates and every historical message, retaining v4 defaults", async () => {
  const ring = await parseKeyring(JSON.stringify(v2));
  expect(ring.activeKeys.map(key => key.info.version)).toEqual([4, 6]);
  expect(ring.activeKey.info.version).toBe(4);
  expect(ring.keys.map(key => key.armored)).toEqual(v2.keys.map(key => key.privateKey));
  unlockWithArmoredKey(JSON.stringify(v2));
  for (const name of ["v4", "v6-pqc"]) expect((await openSealedToSelf(read(`${name}.msg.asc`))).replace(/\r\n/g, "\n")).toBe(read("message.txt"));
  expect((await generateIdentity("Default", "default@example.test")).fingerprint).toHaveLength(40);
});

it("accepts either optional active slot and rejects version-confused, missing or subkey actives", async () => {
  for (const active of [{ v4: v2.active.v4 }, { v6: v2.active.v6 }]) {
    const parsed = await parseKeyring(JSON.stringify({ ...v2, active }));
    expect(parsed.activeKeys).toHaveLength(1);
  }
  for (const active of [{}, { v4: v2.active.v6 }, { v6: v2.active.v4 }, { v5: v2.active.v4 }, { v6: "F".repeat(64) }, { v4: v2.keyFingerprints[1] }, { v6: null }]) {
    await expect(parseKeyring(JSON.stringify({ ...v2, active }))).rejects.toThrow(/invalid/);
  }
  await expect(parseKeyring(JSON.stringify({ ...v2, activeFingerprint: v2.active.v4 }))).rejects.toThrow(/invalid/);
  await expect(parseKeyring(JSON.stringify({ ...v2, keyFingerprints: v2.keyFingerprints.slice(1) }))).rejects.toThrow(/invalid/);
});

it("roundtrips a v2 ring above the old cap through recovery, without losing either active key", async () => {
  const raw = JSON.stringify({ ...v2, keys: v2.keys.map(key => ({ ...key, revocationCertificate: "test".repeat(40 << 10) })) });
  expect(raw.length).toBeGreaterThan(128 << 10);
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring") throw new Error("ring required");
  expect(JSON.stringify(await wrapPrivateKey(raw, "password")).length).toBeLessThan(512 << 10);
  for (const [fingerprint, publicKey] of [[v2.active.v4!, await publicOf(v2.keys[0].privateKey)], [v2.active.v6!, await publicOf(v2.keys[1].privateKey)]]) {
    const snapshot = { fingerprint, publicKey, keyring: ring.metadata };
    const created = await createRecoveryBackup(raw, snapshot);
    expect((await restoreRecoveryBackup(JSON.stringify(created.backup), created.secret)).privateKey).toBe(raw);
    expect((await validateKeyringSnapshot(raw, snapshot)).activeKey.info.fingerprint).toBe(fingerprint);
    await expect(validateKeyringSnapshot(raw, { ...snapshot, publicKey: read("v6.pub.asc") })).rejects.toThrow(/public key/);
  }
}, 30000);

it("delivers exact v2 bytes in a large X-Wing envelope with 64-hex AAD", async () => {
  const raw = JSON.stringify(v2).padEnd(180 << 10);
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring") throw new Error("ring required");
  const fingerprint = v2.active.v6!;
  const request = { raw, deviceId: device.deviceId, publicKeyB64: device.publicKey, version: 6 as const,
    typedCode: await deriveEnrollmentCode(device.publicKey, device.deviceId, bucketFor(Math.floor(Date.now() / 1000))),
    snapshot: { fingerprint, publicKey: await publicOf(v2.keys[1].privateKey), keyring: ring.metadata } };
  const envelope = await sealKeyringForDevice(request);
  expect(JSON.stringify(envelope).length).toBeGreaterThan(128 << 10);
  expect(JSON.stringify(envelope).length).toBeLessThan(512 << 10);
  const input = { envelope, version: 6 as const, privateKey: new Uint8Array(Buffer.from(device.seed, "base64")), deviceId: device.deviceId, fingerprint };
  expect(await openDeviceEnvelope(input)).toBe(raw);
  await expect(openDeviceEnvelope({ ...input, fingerprint: "F".repeat(64) })).rejects.toThrow();
});
