// @vitest-environment node
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { getPGPBootstrap, getRecoveryBackup, upgradePGPToPQC, type PGPBootstrap } from "../api/pgp";
import { generateIdentity } from "./pgpClient";
import { parseKeyring } from "./pgpKeyring";
import { clearPGPSession, loadPGPSession, unlockPGPSession } from "./pgpSession";
import { requireUnlockedKeyMaterial, restoreRecoveryBackup, unwrapPrivateKey, wrapPrivateKey } from "./keyVault";
import { commitPQUpgrade, preparePQUpgrade } from "./pgpUpgrade";
vi.mock("../api/pgp", async original => ({ ...await original<object>(), getPGPBootstrap: vi.fn(), getRecoveryBackup: vi.fn(), upgradePGPToPQC: vi.fn() }));
let snapshot: PGPBootstrap;
beforeEach(async () => {
  const key = await generateIdentity("Owner", "owner@example.test");
  snapshot = { pgpRevision: 2, hasIdentity: true, protection: "client", fingerprint: key.fingerprint, keyId: "", publicKey: key.armoredPublicKey,
    keySource: "generated", createdAt: "now", wrappedPrivateKey: JSON.stringify(await wrapPrivateKey(key.armoredPrivateKey, "password")),
    unlockRequired: true, canDecryptServerSide: false, migrationAvailable: false, signerKeys: [], suggestedUserIDs: ["owner@example.test"], displayName: "Owner" };
  vi.mocked(getPGPBootstrap).mockImplementation(async () => snapshot);
  await loadPGPSession();
  await unlockPGPSession("password");
});
afterEach(() => { clearPGPSession(); vi.clearAllMocks(); });

it("prepares and verifies complete password and recovery rings before any write", async () => {
  const old = requireUnlockedKeyMaterial();
  const prepared = await preparePQUpgrade("password");
  expect(upgradePGPToPQC).not.toHaveBeenCalled();
  expect(requireUnlockedKeyMaterial()).toBe(old);
  const ring = await parseKeyring(prepared.plaintext);
  expect(ring.activeKeys).toHaveLength(2);
  expect(ring.keys[0].armored).toBe(old);
  expect((await restoreRecoveryBackup(JSON.stringify(prepared.backup), prepared.secret)).privateKey).toBe(prepared.plaintext);
  expect(await unwrapPrivateKey(JSON.parse(prepared.request.wrapped), "password")).toBe(prepared.plaintext);
});

it("confirms a committed conversion after a lost response without retrying it", async () => {
  const prepared = await preparePQUpgrade("password");
  const ring = await parseKeyring(prepared.plaintext);
  vi.mocked(upgradePGPToPQC).mockImplementation(async () => {
    snapshot = { ...snapshot, pgpRevision: 3, wrappedPrivateKey: prepared.request.wrapped, publicKeyV6: prepared.request.publicKeyV6,
      keyring: prepared.request.keyring, fingerprintV6: ring.activeKeys[1].info.fingerprint, v6Ready: false };
    throw new Error("response lost");
  });
  vi.mocked(getRecoveryBackup).mockResolvedValue(prepared.backup);
  await commitPQUpgrade(prepared, "password", () => true);
  expect(upgradePGPToPQC).toHaveBeenCalledTimes(1);
  expect(requireUnlockedKeyMaterial()).toBe(prepared.plaintext);
});

it("never uploads after lock, logout or unmount and never installs an unconfirmed ring", async () => {
  const prepared = await preparePQUpgrade("password");
  const old = requireUnlockedKeyMaterial();
  await expect(commitPQUpgrade(prepared, "password", () => false)).rejects.toThrow(/session changed/);
  expect(upgradePGPToPQC).not.toHaveBeenCalled();
  vi.mocked(upgradePGPToPQC).mockRejectedValue(new Error("revision changed"));
  await expect(commitPQUpgrade(prepared, "password", () => true)).rejects.toThrow(/revision changed/);
  expect(requireUnlockedKeyMaterial()).toBe(old);
  clearPGPSession();
  await expect(commitPQUpgrade(prepared, "password", () => true)).rejects.toThrow(/session changed/);
});
