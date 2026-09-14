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
