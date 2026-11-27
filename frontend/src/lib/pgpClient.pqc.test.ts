// @vitest-environment node
import { readFileSync } from "node:fs";
import { afterEach, beforeAll, expect, it, vi } from "vitest";
import { getPGPBootstrap } from "../api/pgp";
import { pgpEngine, type EngineKey } from "./pgpEngine";
import { addPQCertificate, parseKeyring } from "./pgpKeyring";
import { lock, unlockWithArmoredKey } from "./keyVault";
import { unlockedMaterialGeneration, buildEncryptedDeliveries, buildEncryptedDraft, buildEncryptedSentCopy, buildSignedDelivery, decryptMessage, extractArmoredMessage, generateIdentity, splitEncryptionGroups } from "./pgpClient";
vi.mock("../api/pgp", () => ({ getPGPBootstrap: vi.fn() }));
const read = (name: string) => readFileSync(new URL(`../../../testdata/openpgp-v6/${name}`, import.meta.url), "utf8");
let v4: EngineKey, v6: EngineKey, raw: string;
const envelope = { from: "sender@example.test", to: ["pq@example.test", "classical@example.test"], cc: ["pq2@example.test"], subject: "protected subject" };
beforeAll(async () => {
  v4 = await generateIdentity("Sender", envelope.from);
  v6 = await generateIdentity("Sender", envelope.from, ["alias@example.test", envelope.from.toUpperCase(), "alias@example.test"], "v6");
  const result = await addPQCertificate(v4.armoredPrivateKey, { fingerprint: v4.fingerprint, publicKey: v4.armoredPublicKey }, v6);
  raw = result.plaintext;
});
afterEach(() => { lock(); vi.clearAllMocks(); });

// Independent packet framing, before any decrypt: generated PKESKs have fixed
// lengths; the data packet can be partial and only its version octet is read.
async function packets(ciphertext: string) {
  const bytes = await (await pgpEngine()).dearmor(extractArmoredMessage(ciphertext));
  const keys: number[] = [];
  let offset = 0;
  while (offset < bytes.length) {
    const header = bytes[offset++];
    if (!(header & 64)) throw new Error("expected new-format packets");
    const tag = header & 63;
    let length = bytes[offset++];
    if (length >= 192 && length < 224) length = ((length - 192) << 8) + bytes[offset++] + 192;
    else if (length === 255) { length = new DataView(bytes.buffer, bytes.byteOffset + offset, 4).getUint32(0); offset += 4; }
    else if (length >= 224) { if (tag !== 18) throw new Error("partial PKESK"); }
    if (tag === 1) keys.push(bytes[offset]);
    if (tag === 18) return { keys, seipd: bytes[offset] };
    offset += length;
  }
  throw new Error("no encrypted data");
}

it("generates an unprotected 30/35 v6 key and writes both active certificates without changing v4", async () => {
  const engine = await pgpEngine();
  const info = await engine.inspect(v6.armoredPrivateKey);
  expect(info).toMatchObject({ version: 6, locked: false, algorithms: [30, 35] });
  expect(v6.fingerprint).toMatch(/^[A-F0-9]{64}$/);
  const ids = info.packets.filter(packet => (packet[0] & 63) === 13).map(packet => new TextDecoder().decode(packet));
  expect(ids).toHaveLength(2);
  expect(ids.join("")).toContain(envelope.from);
  expect(ids.join("")).toContain("alias@example.test");
  const ring = await parseKeyring(raw);
  expect(ring.activeKeys.map(key => key.info.version)).toEqual([4, 6]);
  expect(ring.keys[0].armored).toBe(v4.armoredPrivateKey);
  await expect(addPQCertificate(raw, { fingerprint: v4.fingerprint, publicKey: v4.armoredPublicKey }, v6)).rejects.toThrow();
});

it("never mixes v6 and v3 PKESKs, signs each group with its matching certificate, and isolates every Bcc", async () => {
  unlockWithArmoredKey(raw);
  const groups = [
    { recipients: [...envelope.to, ...envelope.cc!], publicKeys: [v6.armoredPublicKey, v4.armoredPublicKey, v6.armoredPublicKey] },
    { recipients: ["hidden-pq@example.test"], publicKeys: [v6.armoredPublicKey] },
    { recipients: ["hidden-v4@example.test"], publicKeys: [v4.armoredPublicKey] }
  ];
  const split = await splitEncryptionGroups(groups);
  expect(split.map(group => group.recipients)).toEqual([["pq@example.test", "pq2@example.test"], ["classical@example.test"], ["hidden-pq@example.test"], ["hidden-v4@example.test"]]);
  const deliveries = await buildEncryptedDeliveries(envelope, "text/plain", "secret", groups, true);
  expect(deliveries).toHaveLength(4);
  for (const [index, delivery] of deliveries.entries()) {
    const pq = index === 0 || index === 2;
    const summary = await packets(delivery.ciphertext);
    expect(new Set(summary.keys)).toEqual(new Set([pq ? 6 : 3]));
    expect(summary.seipd).toBe(pq ? 2 : 1);
    const reader = pq ? v6 : v4;
    const wrong = pq ? v4 : v6;
    const message = extractArmoredMessage(delivery.ciphertext);
    const engine = await pgpEngine();
    const opened = await engine.decrypt({ armoredMessage: message, decryptionKeys: [reader.armoredPrivateKey], verificationKeys: [reader.armoredPublicKey], maxBytes: 1 << 20 });
    expect(opened.signatures).toHaveLength(1);
    expect(opened.signatures[0].valid).toBe(true);
    expect(opened.quantumSafe).toBe(pq);
    await expect(engine.decrypt({ armoredMessage: message, decryptionKeys: [wrong.armoredPrivateKey], verificationKeys: [], maxBytes: 1 << 20 })).rejects.toThrow();
    expect(delivery.ciphertext).toContain(`To: ${envelope.to.join(", ")}`);
    expect(delivery.ciphertext).toContain(`Cc: ${envelope.cc!.join(", ")}`);
    expect(delivery.ciphertext).not.toContain("hidden-");
  }
});

it("also isolates a v4 ID35 recipient from classical v4 recipients", async () => {
  const deliveries = await buildEncryptedDeliveries(envelope, "text/plain", "x", [{ recipients: envelope.to,
    publicKeys: [read("v4-pqc.pub.asc"), v4.armoredPublicKey] }], false);
  expect(deliveries).toHaveLength(2);
  expect((await packets(deliveries[0].ciphertext)).keys).toEqual([3]);
});

it("keeps self-encryption v4 until fresh device readiness and refuses a stale ring", async () => {
  unlockWithArmoredKey(raw);
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring") throw new Error("ring required");
  const snapshot = { fingerprint: v4.fingerprint, publicKey: v4.armoredPublicKey, publicKeyV6: v6.armoredPublicKey, keyring: ring.metadata };
  for (const ready of [false, true]) {
    vi.mocked(getPGPBootstrap).mockResolvedValue({ ...snapshot, v6Ready: ready } as Awaited<ReturnType<typeof getPGPBootstrap>>);
    const sent = await buildEncryptedSentCopy(envelope, "text/plain", "sent", true, [], ["classical@example.test"]);
    expect((await packets(sent)).keys).toEqual([ready ? 6 : 3]);
    const opened = await decryptMessage(sent, [], "");
    expect(opened.protectedHeaders.classicalRecipients).toBe("classical@example.test");
    expect(opened.quantumSafe).toBe(ready);
    expect(await unlockedMaterialGeneration()).toBe(ring.metadata.materialGeneration);
    expect((await packets(await buildEncryptedDraft(envelope, "text/plain", "draft"))).seipd).toBe(ready ? 2 : 1);
  }
  vi.mocked(getPGPBootstrap).mockResolvedValue({ ...snapshot, keyring: { ...ring.metadata, materialGeneration: 9 }, v6Ready: true } as Awaited<ReturnType<typeof getPGPBootstrap>>);
  await expect(buildEncryptedSentCopy(envelope, "text/plain", "x", false)).rejects.toThrow(/complete keyring/);
  // Sign-only mail remains classical for outside interoperability.
  expect((await buildSignedDelivery(envelope, "text/plain", "x", envelope.to)).ciphertext).toContain("multipart/signed");
});

it("never borrows a converted generation for a still-unlocked legacy key", async () => {
  unlockWithArmoredKey(v4.armoredPrivateKey);
  vi.mocked(getPGPBootstrap).mockResolvedValue({ keyring: { version: 2, materialGeneration: 1 } } as Awaited<ReturnType<typeof getPGPBootstrap>>);
  expect(await unlockedMaterialGeneration()).toBeUndefined();
});

it("signs sign-only mail with v4 on dual keyrings and preserves v6-only identities", async () => {
  const sigVersion = async (ciphertext: string) => {
    const sig = ciphertext.match(/-----BEGIN PGP SIGNATURE-----[\s\S]+?-----END PGP SIGNATURE-----/)?.[0];
    expect(sig).toBeTruthy();
    const bytes = await (await pgpEngine()).dearmor(sig!);
    const header = bytes[0];
    expect(header & 64).toBeTruthy();
    expect(header & 63).toBe(2);
    const lenOctet = bytes[1];
    const bodyOffset = lenOctet < 192 ? 2 : lenOctet < 224 ? 3 : 6;
    return bytes[bodyOffset];
  };

  unlockWithArmoredKey(raw);
  const dualSigned = await buildSignedDelivery(envelope, "text/plain", "dual", envelope.to);
  expect(dualSigned.ciphertext).toContain("multipart/signed");
  expect(await sigVersion(dualSigned.ciphertext)).toBe(4);

  unlockWithArmoredKey(v6.armoredPrivateKey);
  const v6Signed = await buildSignedDelivery(envelope, "text/plain", "v6-only", envelope.to);
  expect(v6Signed.ciphertext).toContain("multipart/signed");
  expect(await sigVersion(v6Signed.ciphertext)).toBe(6);
});
