import { beforeEach, describe, expect, it } from "vitest";
import { lastRecoveryDrill, recordRecoveryDrill } from "./recoveryDrill";
import type { RecoveryBackup } from "./keyVault";

const backup: RecoveryBackup = {
  format: "kypost-pgp-recovery-v1", fingerprint: "ABCD", publicKey: "PUBLIC",
  envelope: { v: 2, kdf: "PBKDF2-SHA256", iterations: 600000, salt: "AA==", iv: "AA==", ciphertext: "AA==" }
};
beforeEach(() => localStorage.clear());

describe("recovery drill dates", () => {
  it("shows a date only for the exact copy and account that was tested", async () => {
    const date = await recordRecoveryDrill("alice", backup);
    expect(await lastRecoveryDrill("alice", backup)).toBe(date);
    expect(await lastRecoveryDrill("bob", backup)).toBe("");
    expect(await lastRecoveryDrill("alice", { ...backup, fingerprint: "OTHER" })).toBe("");
    expect(await lastRecoveryDrill("alice", { ...backup, envelope: { ...backup.envelope, ciphertext: "BB==" } })).toBe("");
  });
});


it("does not reuse a ring drill date for changed generation or public packets", async () => {
  const fingerprint = "A".repeat(40);
  const ringBackup: RecoveryBackup = { ...backup, format: "kypost-pgp-recovery-v2", fingerprint,
    keyring: { version: 1, materialGeneration: 1, primaryFingerprints: [fingerprint], keyFingerprints: [fingerprint] } };
  const date = await recordRecoveryDrill("alice", ringBackup);
  expect(await lastRecoveryDrill("alice", ringBackup)).toBe(date);
  expect(await lastRecoveryDrill("alice", { ...ringBackup, keyring: { ...ringBackup.keyring, materialGeneration: 2 } })).toBe("");
  expect(await lastRecoveryDrill("alice", { ...ringBackup, publicKey: "updated" })).toBe("");
});
