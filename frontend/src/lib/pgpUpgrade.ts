import { getPGPBootstrap, requirePGPRevision, upgradePGPToPQC } from "../api/pgp";
import { addPQCertificate } from "./pgpKeyring";
import { generateIdentity } from "./pgpClient";
import { createRecoveryBackup, isUnlocked, parseEnvelope, requireUnlockedKeyMaterial, unwrapPrivateKey, wrapPrivateKey } from "./keyVault";
import { acceptCommittedPGPKey, loadPGPSession, unlockedPGPIdentity } from "./pgpSession";

/** Preparation has no writes. Keep its checked recovery file and secret before committing. */
export async function preparePQUpgrade(password: string) {
  const identity = unlockedPGPIdentity();
  const raw = requireUnlockedKeyMaterial();
  const snapshot = await getPGPBootstrap();
  const expectedRevision = requirePGPRevision(snapshot);
  if (expectedRevision !== identity.pgpRevision || snapshot.protection !== "client" || snapshot.publicKeyV6) {
    throw new Error("Your PGP state changed. Reload and unlock the current key before upgrading.");
  }
  const envelope = parseEnvelope(snapshot.wrappedPrivateKey);
  if (!envelope || await unwrapPrivateKey(envelope, password) !== raw) throw new Error("Unlock the current key with this password or passphrase first.");
  const [email, ...aliases] = snapshot.suggestedUserIDs;
  if (!email) throw new Error("Configure your email account before adding its certificate.");
  const generated = await generateIdentity(snapshot.displayName, email, aliases, "v6");
  const { plaintext, metadata } = await addPQCertificate(raw, snapshot, generated);
  const current = { fingerprint: snapshot.fingerprint, publicKey: snapshot.publicKey, publicKeyV6: generated.armoredPublicKey, keyring: metadata };
  const { backup, secret } = await createRecoveryBackup(plaintext, current);
  const wrapped = JSON.stringify(await wrapPrivateKey(plaintext, password));
  if (new TextEncoder().encode(wrapped).length > 512 << 10) throw new Error("The complete keyring exceeds storage capacity. No keys were changed.");
  const isCurrent = () => {
    if (!isUnlocked() || requireUnlockedKeyMaterial() !== raw) return false;
    try { return unlockedPGPIdentity().pgpRevision === expectedRevision; } catch { return false; }
  };
  if (!isCurrent()) throw new Error("The encryption session changed. Prepare the upgrade again.");
  return { plaintext, backup, secret, isCurrent, request: { expectedRevision, expectedFingerprint: snapshot.fingerprint,
    publicKeyV6: generated.armoredPublicKey, wrapped, recoveryEnvelope: JSON.stringify(backup.envelope), keyring: metadata } };
}

export type PreparedPQUpgrade = Awaited<ReturnType<typeof preparePQUpgrade>>;

export async function commitPQUpgrade(prepared: PreparedPQUpgrade, password: string, isMounted: () => boolean) {
  const isCurrent = () => isMounted() && prepared.isCurrent();
  if (!isCurrent()) throw new Error("The encryption session changed. Prepare the upgrade again.");
  let failure: unknown;
  try { await upgradePGPToPQC({ ...prepared.request, credentialSecret: password, isCurrent }); } catch (error) { failure = error; }
  // A response can be lost after commit. Never replay an uncertain mutation:
  // read back its exact password/recovery material and inventory instead.
  if (!isCurrent()) throw new Error("The encryption session changed. Reload to check the upgrade.");
  const current = await getPGPBootstrap();
  if (!isCurrent()) throw new Error("The encryption session changed. Reload to check the upgrade.");
  const { validateKeyringSnapshot } = await import("./pgpKeyring");
  if (current.wrappedPrivateKey !== prepared.request.wrapped || current.keyring == null) {
    if (failure) throw failure;
    throw new Error("Upgrade was not confirmed. Reload before trying again; keep the recovery file.");
  }
  await validateKeyringSnapshot(prepared.plaintext, { ...current, keyring: current.keyring });
  const { getRecoveryBackup } = await import("../api/pgp");
  const recovery = await getRecoveryBackup();
  if (JSON.stringify(recovery.envelope) !== prepared.request.recoveryEnvelope || !isCurrent()) {
    throw new Error("Upgrade recovery copy was not confirmed. Keep the downloaded recovery file and reload.");
  }
  acceptCommittedPGPKey(prepared.plaintext, current);
  await loadPGPSession();
  return current.v6Ready === true;
}
