// @vitest-environment node
import { afterEach, describe, expect, it } from "vitest";
import * as pgp from "openpgp";
import fixture from "../../../testdata/pgp-keyring-v1.json";
import { decryptionKeysFor, parseKeyring } from "./pgpKeyring";
import { createRecoveryBackup, lock, requireUnlockedKey, unlockWithArmoredKey, VaultLockedError } from "./keyVault";
import { buildEncryptedDraft, buildEncryptedSentCopy, buildSignedDelivery, decryptMessage, openSealedToSelf, sealToSelf } from "./pgpClient";

const raw = JSON.stringify(fixture.ring);
const historical = fixture.ring.keys[1];
const active = fixture.ring.keys[0];
const envelope = { from: "fixture@example.invalid", to: ["recipient@example.invalid"], subject: "Old draft" };
afterEach(lock);

describe("keyring readers", () => {
  it("imports a legacy key without converting its bytes", async () => {
    const ring = await parseKeyring(active.privateKey);
    expect(ring.keys).toEqual([ring.activeKey]);
    expect(ring.activeKey.getFingerprint().toUpperCase()).toBe(active.fingerprint);
  });

  it.each(["ciphertext", "hiddenCiphertext"] as const)("reads historical mail and autosaves through %s", async field => {
    const ring = await parseKeyring(raw);
    const message = await pgp.readMessage({ armoredMessage: fixture[field] });
    const candidates = decryptionKeysFor(ring, message);
    expect(candidates.map(k => k.getFingerprint().toUpperCase())).toContain(historical.fingerprint);
    if (field === "ciphertext") {
      expect(candidates).toHaveLength(1);
      expect(message.getEncryptionKeyIDs()[0].toHex()).not.toBe(ring.keys[1].getKeyID().toHex());
    } else expect(candidates).toHaveLength(2);
    unlockWithArmoredKey(active.privateKey);
    await expect(openSealedToSelf(fixture[field])).rejects.toThrow();
    unlockWithArmoredKey(raw);
    expect((await decryptMessage(fixture[field], [], "fixture@example.invalid")).body).toBe(fixture.plaintext);
    expect(await openSealedToSelf(fixture[field])).toBe(fixture.plaintext);
    lock();
    await expect(openSealedToSelf(fixture[field])).rejects.toBeInstanceOf(VaultLockedError);
  });

  it("reopens historical drafts, Sent attachments and autosaves", async () => {
    unlockWithArmoredKey(historical.privateKey);
    const attachments = [{ name: "old.txt", mimeType: "text/plain", dataBase64: btoa("old attachment") }];
    const draft = await buildEncryptedDraft({ ...envelope, bcc: ["hidden@example.invalid"] }, "text/plain", "old body", attachments);
    const sent = await buildEncryptedSentCopy(envelope, "text/plain", "old body", false, attachments);
    const snapshot = await sealToSelf('{"subject":"old autosave"}');
    unlockWithArmoredKey(raw);
    const opened = await decryptMessage(draft, [], envelope.from);
    expect(opened.body).toBe("old body\n");
    expect(opened.protectedHeaders).toMatchObject({ subject: "Old draft", bcc: "hidden@example.invalid" });
    expect(new TextDecoder().decode(opened.attachments[0].bytes)).toBe("old attachment");
    expect((await decryptMessage(sent, [], envelope.from)).attachments[0].name).toBe("old.txt");
    expect(await openSealedToSelf(snapshot)).toBe('{"subject":"old autosave"}');
  });

  it("retains revoked keys for decryption without turning history into signer trust", async () => {
    const old = await pgp.readPrivateKey({ armoredKey: historical.privateKey });
    const signed = await pgp.encrypt({ message: await pgp.createMessage({ text: "signed history" }), encryptionKeys: old.toPublic(), signingKeys: old });
    const revoked = await old.revoke({ flag: pgp.enums.reasonForRevocation.keyCompromised });
    const ring = structuredClone(fixture.ring);
    ring.keys[1].privateKey = revoked.armor();
    unlockWithArmoredKey(JSON.stringify(ring));
    expect(await openSealedToSelf(fixture.ciphertext)).toBe(fixture.plaintext);
    const opened = await decryptMessage(signed, [], envelope.from);
    expect(opened.verified).toBe(false);
  });

  it("refuses all current single-key writers for a ring until lifecycle writes ship", async () => {
    unlockWithArmoredKey(raw);
    expect(() => requireUnlockedKey()).toThrow(/lifecycle upgrade/);
    await expect(createRecoveryBackup(raw, active.fingerprint, "unused")).rejects.toThrow(/lifecycle upgrade/);
    await expect(sealToSelf("new")).rejects.toThrow(/lifecycle upgrade/);
    await expect(buildEncryptedDraft(envelope, "text/plain", "new")).rejects.toThrow(/lifecycle upgrade/);
    await expect(buildEncryptedSentCopy(envelope, "text/plain", "new", false)).rejects.toThrow(/lifecycle upgrade/);
    await expect(buildSignedDelivery(envelope, "text/plain", "new", envelope.to)).rejects.toThrow(/lifecycle upgrade/);
  });

  it.each([
    { ...fixture.ring, format: "kypost-pgp-keyring-v2" },
    { ...fixture.ring, materialGeneration: 0 },
    { ...fixture.ring, materialGeneration: 1.5 },
    { ...fixture.ring, materialGeneration: Number.MAX_SAFE_INTEGER + 1 },
    { ...fixture.ring, activeFingerprint: "missing" },
    { ...fixture.ring, keys: [] },
    { ...fixture.ring, keys: Array(17).fill(active) },
    { ...fixture.ring, keys: [active, active] },
    { ...fixture.ring, keys: [{ ...active, fingerprint: historical.fingerprint }, historical] },
    { ...fixture.ring, keys: [{ ...active, privateKey: "private test sentinel" }, historical] },
    { ...fixture.ring, keys: [{ ...active, revocationCertificate: 1 }, historical] },
    { ...fixture.ring, keyFingerprints: fixture.ring.keyFingerprints.slice(1) },
    { ...fixture.ring, keyFingerprints: [...fixture.ring.keyFingerprints, active.fingerprint] },
    { ...fixture.ring, keyFingerprints: [null] }
  ])("rejects malformed or incomplete ring %# before decrypting", async value => {
    await expect(parseKeyring(JSON.stringify(value))).rejects.toThrow(/^The PGP keyring is invalid or unsupported/);
  });

  it("preserves legacy GnuPG subkey-only decryption with a dummy primary", async () => {
    const key = await pgp.readPrivateKey({ armoredKey: historical.privateKey });
    if (!("makeDummy" in key.keyPacket)) throw new Error("fixture must have a secret primary packet");
    key.keyPacket.makeDummy();
    const exported = key.armor();
    const imported = await pgp.readPrivateKey({ armoredKey: exported });
    expect(imported.isDecrypted()).toBe(true);
    expect((await pgp.decrypt({ message: await pgp.readMessage({ armoredMessage: fixture.ciphertext }), decryptionKeys: imported })).data).toBe(fixture.plaintext);
    unlockWithArmoredKey(exported);
    expect(await openSealedToSelf(fixture.ciphertext)).toBe(fixture.plaintext);
  });

  it("rejects oversized, concatenated, public-only and still-encrypted material", async () => {
    await expect(parseKeyring(" ".repeat(128 << 10) + raw)).rejects.toThrow(/invalid/);
    await expect(parseKeyring(active.privateKey + historical.privateKey)).rejects.toThrow(/invalid/);
    const key = await pgp.readPrivateKey({ armoredKey: active.privateKey });
    await expect(parseKeyring(key.toPublic().armor())).rejects.toThrow(/invalid/);
    const encrypted = await pgp.encryptKey({ privateKey: key, passphrase: "another secret" });
    const ring = structuredClone(fixture.ring);
    ring.keys[0].privateKey = encrypted.armor();
    await expect(parseKeyring(JSON.stringify(ring))).rejects.toThrow(/invalid/);
  });
});
