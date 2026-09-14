// @vitest-environment node
import { beforeAll, describe, expect, it } from "vitest";
import * as pgp from "openpgp";
import fixture from "../../../testdata/pgp-keyring-v1.json";
import { createRecoveryBackup, isUnlocked, lock, restoreRecoveryBackup, unwrapPrivateKey, wrapPrivateKey } from "./keyVault";
import { parseKeyring, parseKeyringMetadata, validateKeyringSnapshot } from "./pgpKeyring";

const raw = JSON.stringify(fixture.ring);
let publicKey: string;
let created: Awaited<ReturnType<typeof createRecoveryBackup>>;
beforeAll(async () => {
  publicKey = (await pgp.readPrivateKey({ armoredKey: fixture.ring.keys[0].privateKey })).toPublic().armor();
  created = await createRecoveryBackup(raw, fixture.ring.activeFingerprint, publicKey);
  lock();
}, 30_000);

describe("complete keyring recovery", () => {
  it("roundtrips every original private packet with the existing v2 wrapper and keeps the vault locked", async () => {
    expect(created.backup.format).toBe("kypost-pgp-recovery-v2");
    expect(created.backup.envelope.iterations).toBe(600_000);
    const restored = await restoreRecoveryBackup(JSON.stringify(created.backup), created.secret);
    expect(restored.privateKey).toBe(raw);
    expect(isUnlocked()).toBe(false);
    expect(JSON.stringify(created.backup)).not.toContain("PRIVATE KEY");
    const parsed = await parseKeyring(restored.privateKey);
    expect(parsed.kind).toBe("keyring");
    if (parsed.kind !== "keyring") throw new Error("expected ring");
    const historical = parsed.keys.find(key => key.getFingerprint().toUpperCase() === fixture.ring.keys[1].fingerprint);
    expect((await pgp.decrypt({ message: await pgp.readMessage({ armoredMessage: fixture.ciphertext }), decryptionKeys: historical })).data).toBe(fixture.plaintext);
  });

  it("preserves an unpublished revocation certificate and exact armor bytes", async () => {
    const key = await pgp.readPrivateKey({ armoredKey: fixture.ring.keys[0].privateKey });
    // Fixture key certificates need not be present; generate a real certificate with reformatKey.
    const generated = await pgp.reformatKey({ privateKey: key, userIDs: [{ email: "fixture@example.invalid" }] });
    const ring = { ...fixture.ring, keys: [{ ...fixture.ring.keys[0], privateKey: generated.privateKey,
      revocationCertificate: generated.revocationCertificate }, fixture.ring.keys[1]] };
    const plaintext = JSON.stringify(ring);
    const backup = await createRecoveryBackup(plaintext, ring.activeFingerprint, generated.publicKey);
    expect((await restoreRecoveryBackup(JSON.stringify(backup.backup), backup.secret)).privateKey).toBe(plaintext);
    expect(JSON.stringify(backup.backup)).not.toContain(generated.revocationCertificate);
  }, 30_000);

  it("accepts normalized inventories but refuses old, truncated, duplicate, or unknown metadata", async () => {
    if (created.backup.format !== "kypost-pgp-recovery-v2") throw new Error("expected v2");
    const keyring = created.backup.keyring;
    await expect(validateKeyringSnapshot(raw, { fingerprint: created.backup.fingerprint.toLowerCase(), publicKey,
      keyring: { ...keyring, primaryFingerprints: keyring.primaryFingerprints.map(x => x.toLowerCase()).reverse(),
        keyFingerprints: keyring.keyFingerprints.map(x => x.toLowerCase()).reverse() } })).resolves.toMatchObject({ kind: "keyring" });
    for (const bad of [
      { ...keyring, materialGeneration: keyring.materialGeneration + 1 },
      { ...keyring, keyFingerprints: keyring.keyFingerprints.slice(1) },
      { ...keyring, primaryFingerprints: keyring.primaryFingerprints.slice(1) },
      { ...keyring, keyFingerprints: [...keyring.keyFingerprints, keyring.keyFingerprints[0].toLowerCase()] },
      { ...keyring, version: 2 }, { ...keyring, materialGeneration: 0 },
      { ...keyring, materialGeneration: Number.MAX_SAFE_INTEGER + 1 },
      { ...keyring, keyFingerprints: Array(257).fill(keyring.keyFingerprints[0]) }
    ]) await expect(validateKeyringSnapshot(raw, { fingerprint: created.backup.fingerprint, publicKey, keyring: bad })).rejects.toThrow();
    expect(() => parseKeyringMetadata(null)).toThrow();
  });

  it("authenticates metadata against sealed material and refuses relabeling a ring as legacy", async () => {
    if (created.backup.format !== "kypost-pgp-recovery-v2") throw new Error("expected v2");
    const tampered = { ...created.backup, keyring: { ...created.backup.keyring, materialGeneration: created.backup.keyring.materialGeneration + 1 } };
    await expect(restoreRecoveryBackup(JSON.stringify(tampered), created.secret)).rejects.toThrow(/complete keyring/);
    await expect(restoreRecoveryBackup(JSON.stringify({ ...created.backup, format: "kypost-pgp-recovery-v1" }), created.secret)).rejects.toThrow(/lifecycle upgrade/);
    await expect(restoreRecoveryBackup(JSON.stringify(created.backup), "FFFF-FFFF-FFFF-FFFF-FFFF-FFFF-FFFF-FFFF")).rejects.toThrow(/did not unlock/);
  }, 30_000);

  it("compares full public packets, accepts UID ordering, and refuses changed revocations", async () => {
    const key = await pgp.readPrivateKey({ armoredKey: fixture.ring.keys[0].privateKey });
    const multi = await pgp.reformatKey({ privateKey: key, userIDs: [{ email: "one@example.invalid" }, { email: "two@example.invalid" }, { email: "three@example.invalid" }] });
    const ring = { ...fixture.ring, keys: [{ ...fixture.ring.keys[0], privateKey: multi.privateKey }, fixture.ring.keys[1]] };
    const backup = await createRecoveryBackup(JSON.stringify(ring), ring.activeFingerprint, multi.publicKey);
    if (backup.backup.format !== "kypost-pgp-recovery-v2") throw new Error("expected v2");
    const reordered = await pgp.readKey({ armoredKey: multi.publicKey });
    reordered.users.reverse();
    await expect(validateKeyringSnapshot(JSON.stringify(ring), { ...backup.backup, publicKey: reordered.armor() })).resolves.toBeTruthy();
    const revoked = await (await pgp.readPrivateKey({ armoredKey: multi.privateKey })).revoke({ flag: pgp.enums.reasonForRevocation.keyCompromised });
    await expect(validateKeyringSnapshot(JSON.stringify(ring), { ...backup.backup, publicKey: revoked.toPublic().armor() })).rejects.toThrow(/revocations may have changed/);
    await expect(validateKeyringSnapshot(JSON.stringify(ring), { ...backup.backup, publicKey: multi.privateKey })).rejects.toThrow(/public key/);
  }, 30_000);

  it("enforces the sealed-envelope cap including base64 expansion before offering a file", async () => {
    const large = JSON.stringify({ ...fixture.ring, keys: [{ ...fixture.ring.keys[0], revocationCertificate: "x".repeat(99 << 10) }, fixture.ring.keys[1]] });
    expect(new TextEncoder().encode(large).length).toBeLessThan(128 << 10);
    await expect(createRecoveryBackup(large, fixture.ring.activeFingerprint, publicKey)).rejects.toThrow(/envelope capacity/);
    await expect(createRecoveryBackup(" ".repeat(128 << 10) + raw, fixture.ring.activeFingerprint, publicKey)).rejects.toThrow(/invalid/);
    if (created.backup.format !== "kypost-pgp-recovery-v2") throw new Error("expected v2");
    await expect(restoreRecoveryBackup(JSON.stringify({ ...created.backup, envelope: { ...created.backup.envelope, ciphertext: "A".repeat(128 << 10) } }), created.secret)).rejects.toThrow(/envelope capacity/);
  }, 30_000);

  it("does not lose history during password wrapping and does not alter material generation", async () => {
    const envelope = await wrapPrivateKey(raw, "new-password");
    const opened = await unwrapPrivateKey(envelope, "new-password");
    expect(opened).toBe(raw);
    expect(JSON.parse(opened).materialGeneration).toBe(fixture.ring.materialGeneration);
    expect(isUnlocked()).toBe(false);
  }, 30_000);
});
