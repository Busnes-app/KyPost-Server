import type { RecoveryBackup } from "./keyVault";

// ponytail: dates are browser-local, not an account-wide audit history. Add
// server metadata only if cross-device drill history becomes a requirement.
function storageKey(userId: string, backup: RecoveryBackup): string {
  if (!userId) throw new Error("Sign in again before recording a recovery drill.");
  return `kypost-pgp-drill:${encodeURIComponent(userId)}:${backup.fingerprint.toUpperCase()}`;
}

async function envelopeHash(backup: RecoveryBackup): Promise<string> {
  const hash = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(JSON.stringify(backup.format === "kypost-pgp-recovery-v2" ? backup : backup.envelope)));
  return Array.from(new Uint8Array(hash), (b) => b.toString(16).padStart(2, "0")).join("");
}

/** Call only after opening this copy and matching every parsed key and public packet to a fresh identity snapshot. */
export async function recordRecoveryDrill(userId: string, backup: RecoveryBackup): Promise<string> {
  const hash = await envelopeHash(backup);
  const date = new Date().toISOString();
  localStorage.setItem(storageKey(userId, backup), JSON.stringify({ hash, date }));
  return date;
}

export async function lastRecoveryDrill(userId: string, backup: RecoveryBackup): Promise<string> {
  const hash = await envelopeHash(backup);
  const raw = localStorage.getItem(storageKey(userId, backup));
  if (!raw) return "";
  const record: unknown = JSON.parse(raw);
  if (record && typeof record === "object" && "hash" in record && record.hash === hash &&
      "date" in record && typeof record.date === "string" && Number.isFinite(Date.parse(record.date))) {
    return record.date;
  }
  return "";
}
