// @vitest-environment node
import { afterEach, beforeAll, describe, expect, it } from "vitest";
import * as pgp from "openpgp";
import {
  buildEncryptedDeliveries, buildEncryptedDraft, buildEncryptedSentCopy,
  buildSignedDelivery, decryptMessage, extractArmoredMessage, importIdentity,
  openSealedToSelf, sealToSelf, verifySignedMessage
} from "./pgpClient";
import { lock, unlockWithArmoredKey } from "./keyVault";

const envelope = { from: "alice@example.com", to: ["bob@example.com"], subject: "Private subject" };
type Identity = { publicKey: string; privateKey: string };
let weak: Identity;
let old: Identity;
let strong: Identity;
let weakSubkey: Identity;

async function rsa(bits: number, subkeyBits = bits): Promise<Identity> {
  const key = await pgp.generateKey({
    type: "rsa", rsaBits: bits,
    subkeys: [{ type: "rsa", rsaBits: subkeyBits }],
    userIDs: [{ name: "Public test fixture", email: envelope.from }],
    format: "armored",
    // Fixture creation only: production operations must refuse these keys.
    config: { minRSABits: 1024 }
  });
  const parsed = await pgp.readKey({ armoredKey: key.publicKey });
  expect(parsed.getAlgorithmInfo().bits).toBe(bits);
  expect(parsed.subkeys[0].getAlgorithmInfo().bits).toBe(subkeyBits);
  return key;
}

beforeAll(async () => {
  old = await rsa(1024);
  weak = await rsa(2047);
  strong = await rsa(2048);
  weakSubkey = await rsa(2048, 2047);
}, 60000);
afterEach(lock);

function deliveries(key: Identity, sign = false) {
  return buildEncryptedDeliveries(envelope, "text/plain", "Private body",
    [{ recipients: envelope.to, publicKeys: [key.publicKey] }], sign);
}

describe("browser OpenPGP RSA floor", () => {
  it("refuses RSA-2047 recipient encryption", async () => {
    await expect(deliveries(weak)).rejects.toThrow(/2048/);
  });

  it("refuses an RSA-2047 encryption subkey under a strong primary", async () => {
    await expect(deliveries(weakSubkey)).rejects.toThrow(/2048/);
  });

  it("does not omit a weak recipient when another recipient is strong", async () => {
    await expect(buildEncryptedDeliveries(envelope, "text/plain", "Private body",
      [{ recipients: envelope.to, publicKeys: [strong.publicKey, weak.publicKey] }], false))
      .rejects.toThrow(/2048/);
  });

  it("refuses RSA-2047 signing of encrypted deliveries", async () => {
    unlockWithArmoredKey(weak.privateKey);
    await expect(deliveries(strong, true)).rejects.toThrow(/2048/);
  });

  it("refuses RSA-2047 detached signing", async () => {
    unlockWithArmoredKey(weak.privateKey);
    await expect(buildSignedDelivery(envelope, "text/plain", "Private body", envelope.to))
      .rejects.toThrow(/2048/);
  });

  for (const path of ["Sent copy", "draft", "autosave"] as const) {
    it(`refuses RSA-2047 for a new ${path}`, async () => {
      unlockWithArmoredKey(weak.privateKey);
      const result = path === "Sent copy"
        ? buildEncryptedSentCopy(envelope, "text/plain", "Private body", false)
        : path === "draft" ? buildEncryptedDraft(envelope, "text/plain", "Private body")
          : sealToSelf("Private body");
      await expect(result).rejects.toThrow(/2048/);
    });
  }

  it("keeps RSA-2048 delivery, signing, Sent, drafts and autosave usable", async () => {
    unlockWithArmoredKey(strong.privateKey);
    const encrypted = await deliveries(strong, true);
    const opened = await decryptMessage(encrypted[0].ciphertext,
      [{ addresses: [envelope.from], publicKey: strong.publicKey }], envelope.from);
    expect(opened.body).toBe("Private body\n");
    expect(opened.verified).toBe(true);
    const signed = await buildSignedDelivery(envelope, "text/plain", "Private body", envelope.to);
    expect(signed.ciphertext).toContain("BEGIN PGP SIGNATURE");
    for (const ciphertext of [
      await buildEncryptedSentCopy(envelope, "text/plain", "Private body", true),
      await buildEncryptedDraft(envelope, "text/plain", "Private body"),
      await sealToSelf("Private body")
    ]) {
      expect(await openSealedToSelf(extractArmoredMessage(ciphertext))).toContain("Private body");
    }
  });

  for (const bits of [1024, 2047]) {
    it(`preserves historical RSA-${bits} mail, draft and autosave decryption`, async () => {
      const retained = bits === 1024 ? old : weak;
      const privateKey = await pgp.readPrivateKey({ armoredKey: retained.privateKey });
      const ciphertext = await pgp.encrypt({
        message: await pgp.createMessage({ text: "Historical private text" }),
        encryptionKeys: privateKey.toPublic(), format: "armored", config: { minRSABits: 1024 }
      });
      unlockWithArmoredKey(retained.privateKey);
      expect((await decryptMessage(ciphertext, [], "")).body).toBe("Historical private text");
      expect(await openSealedToSelf(ciphertext)).toBe("Historical private text");
      const imported = await importIdentity(retained.privateKey, "");
      expect(imported.armoredPrivateKey).toBe(privateKey.armor());
    });
  }

  it("reads encrypted mail without trusting an RSA-2047 sender signature", async () => {
    const ciphertext = await pgp.encrypt({
      message: await pgp.createMessage({ text: "Historical signed text" }),
      encryptionKeys: await pgp.readKey({ armoredKey: strong.publicKey }),
      signingKeys: await pgp.readPrivateKey({ armoredKey: weak.privateKey }), format: "armored"
    });
    unlockWithArmoredKey(strong.privateKey);
    const result = await decryptMessage(ciphertext,
      [{ addresses: [envelope.from], publicKey: weak.publicKey }], envelope.from);
    expect(result.body).toBe("Historical signed text");
    expect(result.signed).toBe(true);
    expect(result.verified).toBe(false);
  });

  it("reads detached signed mail without trusting an RSA-2047 sender signature", async () => {
    const text = "Content-Type: text/plain\r\n\r\nHistorical signed text";
    const signature = await pgp.sign({
      message: await pgp.createMessage({ binary: new TextEncoder().encode(text) }),
      signingKeys: await pgp.readPrivateKey({ armoredKey: weak.privateKey }),
      detached: true, format: "armored"
    });
    const result = await verifySignedMessage(btoa(text), signature,
      [{ addresses: [envelope.from], publicKey: weak.publicKey }], envelope.from);
    expect(result.body).toBe("Historical signed text");
    expect(result.signed).toBe(true);
    expect(result.verified).toBe(false);
  });
});
