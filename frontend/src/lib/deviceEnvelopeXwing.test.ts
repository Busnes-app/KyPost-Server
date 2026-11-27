// @vitest-environment node
import { expect, it } from "vitest";
import { writeFileSync } from "node:fs";
import fixture from "../../../testdata/device-envelope-xwing.json";
import legacy from "../../../testdata/device-envelope-hpke.json";
import { bucketFor, deriveEnrollmentCode, deviceEnvelopeVersion, openDeviceEnvelope, sealEnvelopeForDevice, sealKeyringForDevice } from "./deviceEnrollment";
import { loadBridge } from "./pgpEngineWasm";
import { parseKeyring } from "./pgpKeyring";

const b = (s: string) => new Uint8Array(Buffer.from(s, "base64"));
const input = { version: 6, privateKey: b(fixture.seed), deviceId: fixture.deviceId, fingerprint: fixture.fingerprint } satisfies Omit<Parameters<typeof openDeviceEnvelope>[0], "envelope">;

it("opens the Go v6 vector through real WASM and preserves the v5 opener", async () => {
  expect(await openDeviceEnvelope({ ...input, envelope: fixture.envelope })).toBe(fixture.plaintext);
  const v5 = legacy.vectors[1];
  expect(await openDeviceEnvelope({ version: 5, privateKey: b(legacy.devicePrivateKey), envelope: v5.envelope, deviceId: v5.deviceId, fingerprint: v5.fingerprint })).toBe(v5.plaintext);
});

it("SAS v2 agrees with Go and binds every one of the 1216 key bytes", async () => {
  expect(await deriveEnrollmentCode(fixture.publicKey, fixture.deviceId, fixture.bucket)).toBe(fixture.sas);
  for (let i = 0; i < 1216; i++) {
    const changed = b(fixture.publicKey); changed[i] ^= 1;
    expect(await deriveEnrollmentCode(Buffer.from(changed).toString("base64"), fixture.deviceId, fixture.bucket), `byte ${i}`).not.toBe(fixture.sas);
  }
});

it("seals through WASM for Go to open; refuses changed binding and malformed frames", async () => {
  const envelope = await sealEnvelopeForDevice(fixture.publicKey, fixture.deviceId, fixture.fingerprint, fixture.plaintext);
  expect(envelope.v).toBe(6);
  expect(b(envelope.enc)).toHaveLength(1120);
  expect(await openDeviceEnvelope({ ...input, envelope })).toBe(fixture.plaintext);
  if (process.env.KYPOST_WRITE_XWING_BROWSER_FIXTURE === "1") {
    writeFileSync(new URL("../../../testdata/device-envelope-xwing-browser.json", import.meta.url), JSON.stringify(envelope, null, 2) + "\n");
  }
  for (const override of [{deviceId:"other"}, {fingerprint:"F".repeat(40)}, {version:5}] satisfies Partial<typeof input | { version: 5 }>[]) {
    await expect(openDeviceEnvelope({ ...input, envelope, ...override })).rejects.toThrow();
  }
  const flipped = b(envelope.ct); flipped[0] ^= 1;
  for (const bad of [
    {...envelope, enc:Buffer.alloc(65).toString("base64")},
    {...envelope, alg:"HPKE-Base-P256-SHA256-A256GCM"},
    {...envelope, enc:envelope.enc.replace(/=$/, "")},
    {...envelope, ct:Buffer.from(flipped).toString("base64")},
    {...envelope, iv:""},
  ]) await expect(openDeviceEnvelope({ ...input, envelope:bad })).rejects.toThrow();
  await expect(sealEnvelopeForDevice(Buffer.alloc(1216,255).toString("base64"), fixture.deviceId, fixture.fingerprint, fixture.plaintext)).rejects.toThrow();
  expect(deviceEnvelopeVersion(true, [4,5,6])).toBe(6);
  expect(deviceEnvelopeVersion(false, [6])).toBe(6);
});

it("delivers the complete validated keyring in v6 and gates it on SAS v2", async () => {
  const raw = legacy.vectors[1].plaintext;
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring") throw new Error("fixture must be a ring");
  const bridge = await loadBridge();
  const key = bridge.inspect({ key: ring.activeKey.armored });
  if (!(key.publicKey instanceof Uint8Array)) throw new Error("fixture public key");
  const armor = bridge.armor({ data:key.publicKey, type:"PGP PUBLIC KEY BLOCK" });
  if (typeof armor.armored !== "string") throw new Error("fixture public armor");
  const req = { publicKeyB64:fixture.publicKey, deviceId:fixture.deviceId, raw, typedCode:await deriveEnrollmentCode(fixture.publicKey,fixture.deviceId,bucketFor(Math.floor(Date.now()/1000))), snapshot:{fingerprint:ring.activeKey.info.fingerprint,publicKey:armor.armored,keyring:ring.metadata} };
  const envelope = await sealKeyringForDevice(req);
  expect(envelope.v).toBe(6);
  expect(await openDeviceEnvelope({ ...input, fingerprint:ring.activeKey.info.fingerprint, envelope })).toBe(raw);
  await expect(sealKeyringForDevice({...req,typedCode:"0".repeat(14)})).rejects.toThrow(/code/);
});

it("generates fresh X-Wing device keys in WASM, without persisting private seeds", async () => {
  const bridge = await loadBridge();
  const generated = bridge.xwingGenerate({});
  if (!(generated.seed instanceof Uint8Array) || !(generated.publicKey instanceof Uint8Array)) throw new Error("key generation failed");
  try {
    expect(generated.seed).toHaveLength(32); expect(generated.publicKey).toHaveLength(1216);
    const envelope = await sealEnvelopeForDevice(Buffer.from(generated.publicKey).toString("base64"), input.deviceId, input.fingerprint, "fresh key");
    expect(await openDeviceEnvelope({...input, privateKey:new Uint8Array(generated.seed),envelope})).toBe("fresh key");
  } finally { generated.seed.fill(0); }
});
