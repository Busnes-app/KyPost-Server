import { ChangeEvent, FormEvent, useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import { toErrorMessage } from "../../../api/client";
import {
  deletePGPIdentity,
  getPGPBootstrap,
  requirePGPRevision,
  getRecoveryBackup,
  putRecoveryEnvelope,
  storeClientPGPIdentity,
  rewrapPGPPrivateKey,
  exportLegacyPGPKey,
  getPGPDiscoverySettings,
  updatePGPDiscoverySettings,
  listDiscoverySuppressions,
  removeDiscoverySuppression,
  type PGPIdentity,
  type DiscoverySettings,
  type DiscoverySuppression
} from "../../../api/pgp";
import { validateKeyringSnapshot } from "../../../lib/pgpKeyring";
import { generateIdentity, importIdentity } from "../../../lib/pgpClient";
import {
  createRecoveryBackup,
  requireUnlockedKey,
  requireUnlockedKeyMaterial,
  restoreRecoveryBackup,
  wrapPrivateKey,
  type RecoveryBackup
} from "../../../lib/keyVault";
import {
  lockPGPSession,
  acceptCommittedPGPKey,
  restorePGPKeyring,
  unlockedPGPIdentity,
  loadPGPSession,
  rewrapUnlockedKeyUnder,
  unlockPGPSession,
  type PGPSessionState
} from "../../../lib/pgpSession";
import { listContacts, type Contact } from "../../../api/contacts";

import { useAuth } from "../../../auth";
import { lastRecoveryDrill, recordRecoveryDrill } from "../../../lib/recoveryDrill";

export type PreparedRecoveryBackup = { backup: RecoveryBackup; expectedRevision: number };

const noop = () => {};

export type MailKeysProps = {
  /** Optional so this renders (as "loading") with zero props. */
  pgpIdentity?: PGPIdentity | null;
  setPgpIdentity?: (identity: PGPIdentity | null) => void;
  pgpLoading?: boolean;
  pgpSession?: PGPSessionState | null;
  /** Opens SecurityPage's page-level PgpUnlockDialog. */
  setUnlockOpen?: (open: boolean) => void;
  // Lifted to SecurityPage rather than local: this is the one-time secret
  // that opens a just-downloaded PGP recovery backup, shown exactly once and
  // never re-derivable. Before this split, SecurityPage never unmounted on a
  // tab switch, so the reveal survived one; now the tab strip can unmount
  // this component mid-display, and a local copy would be silently destroyed
  // by switching to Sign-in or Devices and back. Lifting it, like
  // recoveryCodes on SignIn, means it is simply still there when this
  // remounts.
  recoveryBackup?: PreparedRecoveryBackup | null;
  setRecoveryBackup?: (backup: PreparedRecoveryBackup | null) => void;
  recoverySecret?: string;
  setRecoverySecret?: (secret: string) => void;
};

export function MailKeys({
  pgpIdentity = null,
  setPgpIdentity = noop,
  pgpLoading = false,
  pgpSession = null,
  setUnlockOpen = noop,
  recoveryBackup: preparedRecovery = null,
  setRecoveryBackup = noop,
  recoverySecret = "",
  setRecoverySecret = noop
}: MailKeysProps = {}) {
  const { userId } = useAuth();
  const recoveryBackup = preparedRecovery?.backup ?? null;
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);
  const [pgpBusy, setPgpBusy] = useState(false);
  const [pgpStatus, setPgpStatus] = useState("");
  const [pgpImportOpen, setPgpImportOpen] = useState(false);
  const [pgpImportKey, setPgpImportKey] = useState("");
  const [pgpImportPassphrase, setPgpImportPassphrase] = useState("");
  const [migratePassword, setMigratePassword] = useState("");
  const [migrateOpen, setMigrateOpen] = useState(false);
  // Backing out a server-custody key before it becomes unrecoverable. Needs its
  // own password prompt because export-legacy re-verifies the account
  // credential, and its own open flag so it does not share the migrate form.
  const [legacyBackupOpen, setLegacyBackupOpen] = useState(false);
  const [legacyBackupPassword, setLegacyBackupPassword] = useState("");

  // Stale-envelope recovery: the stored PGP envelope is sealed under an OLDER
  // password than the account's, so nothing can open it with the current one.
  // Two passwords are needed to fix it — the old one to unlock, the current one
  // to re-seal.
  const [recoverOpen, setRecoverOpen] = useState(false);
  const [recoverOldPassword, setRecoverOldPassword] = useState("");
  const [recoverCurrentPassword, setRecoverCurrentPassword] = useState("");
  const [restoreSource, setRestoreSource] = useState<{ kind: "file"; file: File } | { kind: "server" } | null>(null);
  const [drillDate, setDrillDate] = useState("");
  const [restoreSecret, setRestoreSecret] = useState("");
  const [restorePassword, setRestorePassword] = useState("");
  const [selfContact, setSelfContact] = useState<Contact | null>(null);

  // PGP key-discovery settings.
  const [discoverySettings, setDiscoverySettings] = useState<DiscoverySettings | null>(null);
  const [discoveryBusy, setDiscoveryBusy] = useState(false);
  const [discoveryStatus, setDiscoveryStatus] = useState("");
  const [suppressions, setSuppressions] = useState<DiscoverySuppression[]>([]);

  useEffect(() => {
    let cancelled = false;
    getPGPDiscoverySettings()
      .then((settings) => {
        if (!cancelled) setDiscoverySettings(settings);
      })
      .catch(() => {
        if (!cancelled) setDiscoverySettings(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    listDiscoverySuppressions()
      .then((r) => {
        if (!cancelled) setSuppressions(r.suppressions);
      })
      .catch(() => {
        if (!cancelled) setSuppressions([]);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  async function allowDiscoveryAgain(email: string) {
    try {
      await removeDiscoverySuppression(email);
      setSuppressions((prev) => prev.filter((s) => s.email !== email));
    } catch {
      setDiscoveryStatus("Failed to update discovery opt-outs.");
    }
  }

  async function updateDiscoverySetting(patch: Partial<DiscoverySettings>) {
    if (!discoverySettings) return;
    const next = { ...discoverySettings, ...patch };
    setDiscoverySettings(next);
    setDiscoveryBusy(true);
    setDiscoveryStatus("");
    try {
      const saved = await updatePGPDiscoverySettings(next);
      setDiscoverySettings(saved);
    } catch (e) {
      setDiscoverySettings(discoverySettings);
      setDiscoveryStatus(`Failed to save: ${toErrorMessage(e, "unknown error")}`);
    } finally {
      setDiscoveryBusy(false);
    }
  }

  useEffect(() => {
    let cancelled = false;
    listContacts()
      .then((all) => {
        if (!cancelled) setSelfContact(all.find((c) => c.isSelf) ?? null);
      })
      .catch(() => {
        if (!cancelled) setSelfContact(null);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  /**
   * Generates the keypair in the browser and uploads only the public half
   * plus an envelope wrapped under the account password. The server never
   * sees the private key, which is the whole point — so this needs the
   * password here, at creation, not just a session.
   */
  async function handleGeneratePGPIdentity() {
    const password = window.prompt(
      "Enter your account password.\n\nYour new private key will be encrypted with it before it leaves this browser. " +
        "This server will not be able to decrypt it — keep a backup of the key."
    );
    if (!password) {
      return;
    }
    setPgpBusy(true);
    setPgpStatus("");
    try {
      // Addresses come from the server, which knows the IMAP account address
      // and every verified send-as alias. Guessing here would mint a key that
      // WKD and Autocrypt then refuse to serve.
      const expectedRevision = requirePGPRevision(pgpSession?.bootstrap);
      const session = await loadPGPSession();
      const addresses = session.bootstrap?.suggestedUserIDs ?? [];
      if (addresses.length === 0) {
        setPgpStatus("Configure your mail account first — the key needs your email address as its User ID.");
        return;
      }
      const name = selfContact?.fn?.trim() || session.bootstrap?.displayName || "KyPost user";
      const generated = await generateIdentity(name, addresses[0], addresses.slice(1));
      const wrapped = await wrapPrivateKey(generated.armoredPrivateKey, password);
      // The same password is the step-up credential: replacing an existing
      // identity needs the account password, not just a session.
      const id = await storeClientPGPIdentity(
        generated.armoredPublicKey,
        JSON.stringify(wrapped),
        "generated",
        password,
        expectedRevision
      );
      // Hold the fresh key for this page so the user is not immediately asked
      // to unlock a key they just made.
      acceptCommittedPGPKey(generated.armoredPrivateKey, id);
      setPgpIdentity(id);
      await loadPGPSession();
      setPgpStatus("New PGP identity generated. Create a recovery copy and keep its secret before you need a password reset.");
    } catch (e) {
      setPgpStatus(`Failed to generate identity: ${toErrorMessage(e, "unknown error")}`);
    } finally {
      setPgpBusy(false);
    }
  }

  /**
   * Migrates a legacy server-held key: the server hands it back once (after
   * re-verifying the password), the browser rewraps it, and the
   * server-readable copy is deleted by the upload.
   */
  async function handleMigrateToClientProtection(e: FormEvent) {
    e.preventDefault();
    setPgpBusy(true);
    setPgpStatus("");
    try {
      const exported = await exportLegacyPGPKey(migratePassword);
      const wrapped = await wrapPrivateKey(exported.privateKey, migratePassword);
      const id = await storeClientPGPIdentity(
        exported.publicKey,
        JSON.stringify(wrapped),
        "imported",
        migratePassword,
        requirePGPRevision(exported)
      );
      acceptCommittedPGPKey(exported.privateKey, id);
      setPgpIdentity(id);
      setMigrateOpen(false);
      setMigratePassword("");
      await loadPGPSession();
      setPgpStatus(
        "Migrated. This server can no longer read your encrypted mail. Create a recovery copy and keep its secret before you need a password reset."
      );
    } catch (err) {
      setPgpStatus(`Migration failed: ${toErrorMessage(err, "unknown error")}`);
    } finally {
      setPgpBusy(false);
    }
  }

  /**
   * Recovers a PGP envelope that is out of step with the account password.
   *
   * This is reachable when a password change committed but the matching rewrap
   * did not — which used to be a permanent loss. The rewrap was a second HTTP
   * request fired after the password write, so a dropped connection between them
   * left the envelope sealed under a password the user no longer had, and every
   * rewrap path re-derived from the CURRENT password and therefore could never
   * open it. The only escape was deleting the identity, losing every message ever
   * encrypted to it.
   *
   * The two writes are atomic now (one request — see LoginPage), so this should
   * never be needed again. It exists for accounts already stranded by the old
   * flow, and because "should never happen" is not a recovery plan.
   */
  async function handleRecoverStaleEnvelope(e: FormEvent) {
    e.preventDefault();
    setPgpBusy(true);
    setPgpStatus("");
    try {
      // Unlock with the OLD password — this only touches memory.
      await unlockPGPSession(recoverOldPassword);
      // Then re-seal under the current one and upload.
      await rewrapUnlockedKeyUnder(recoverCurrentPassword);
      setRecoverOpen(false);
      setRecoverOldPassword("");
      setRecoverCurrentPassword("");
      setPgpStatus("Your PGP key is re-encrypted under your current password.");
    } catch (err) {
      setPgpStatus(
        `Recovery failed: ${toErrorMessage(err, "unknown error")}. Check that the first password is the one your key was last encrypted under.`
      );
    } finally {
      setPgpBusy(false);
    }
  }

  /**
   * Writes a recovery backup to disk and reveals the secret that opens it.
   *
   * Shared by both custody modes, which differ only in where the armored key
   * came from — the browser's own vault, or a one-time export of the copy the
   * server still holds. The file format, the one-time secret and the warning
   * that follows must not differ between them.
   */
  function saveRecoveryBackup(backup: RecoveryBackup, fingerprint: string, secret: string, expectedRevision: number) {
    // Reveal before any fallible download/upload, including a lost PUT response.
    setRecoveryBackup({ backup, expectedRevision });
    setRecoverySecret(secret);
    const url = URL.createObjectURL(new Blob([JSON.stringify(backup)], { type: "application/json" }));
    const link = document.createElement("a");
    link.href = url;
    link.download = `kypost-pgp-recovery-${fingerprint.slice(0, 8)}.json`;
    document.body.appendChild(link);
    link.click();
    link.remove();
    setTimeout(() => URL.revokeObjectURL(url), 60_000);
  }

  async function handleDownloadRecoveryBackup() {
    setPgpBusy(true);
    setPgpStatus("");
    try {
      const snapshot = unlockedPGPIdentity();
      const current = await getPGPBootstrap();
      if (requirePGPRevision(current) !== snapshot.pgpRevision || current.protection !== "client") {
        throw new Error("Your PGP state changed. Reload and unlock the current key before making a backup.");
      }
      if (current.keyring != null) {
        const raw = requireUnlockedKeyMaterial();
        const { backup, secret } = await createRecoveryBackup(raw, current);
        const fresh = await getPGPBootstrap();
        if (requirePGPRevision(fresh) !== snapshot.pgpRevision || fresh.protection !== "client") {
          throw new Error("Your PGP state changed while preparing the backup. Reload and unlock the current key.");
        }
        if (!mounted.current) return;
        saveRecoveryBackup(backup, backup.fingerprint, secret, snapshot.pgpRevision);
        setPgpStatus("Complete keyring recovery file checked. Download requested; keep its secret separately. It cannot recover keys added after this backup.");
        return;
      }
      const imported = await importIdentity(requireUnlockedKey(), "");
      if (imported.fingerprint.toUpperCase() !== snapshot.fingerprint.toUpperCase()) {
        throw new Error("Your PGP identity changed. Reload and unlock the current key before making a backup.");
      }
      const { backup, secret } = await createRecoveryBackup(
        imported.armoredPrivateKey, { fingerprint: imported.fingerprint, publicKey: imported.armoredPublicKey }
      );
      if (!mounted.current) return; // Do not start a download after leaving during creation.
      saveRecoveryBackup(backup, backup.fingerprint, secret, snapshot.pgpRevision);
      setPgpStatus("Recovery file checked in memory. Download requested; check that the file arrived and store the secret separately before saving a server copy.");
    } catch (err) {
      setPgpStatus(`Backup failed: ${toErrorMessage(err, "unlock your key first")}`);
    } finally {
      setPgpBusy(false);
    }
  }

  async function handleStoreRecoveryBackup() {
    if (!recoveryBackup || !recoverySecret || !preparedRecovery) return;
    if (recoveryBackup.format === "kypost-pgp-recovery-v2") {
      setPgpStatus("Complete keyring uploads require the lifecycle update. Keep the downloaded file and secret.");
      return;
    }
    const password = window.prompt(
      "Enter your account password to store the recovery copy.\n\n" +
      "You have confirmed that you saved its secret. This replaces any previous server recovery copy and its secret. Older downloaded files still work with their own secrets."
    );
    if (!password) return;
    setPgpBusy(true);
    setPgpStatus("");
    try {
      await putRecoveryEnvelope(recoveryBackup.envelope, password, recoveryBackup.fingerprint, preparedRecovery.expectedRevision);
      await loadPGPSession();
      setPgpStatus("Recovery copy saved on the server. Keep the file and secret for server loss or identity deletion.");
    } catch (err) {
      setPgpStatus(`Server recovery storage could not be confirmed: ${toErrorMessage(err, "try again")}. Keep this file and secret; the server may already hold the new copy.`);
    } finally {
      setPgpBusy(false);
    }
  }

  /**
   * The same backup for a key the server still holds.
   *
   * A server-custody key is recoverable by an admin, so the backup buys less
   * here than it does under client custody — but it is the only copy that
   * survives the migration cliff. Migrating puts the key beyond the server's
   * reach, and from that moment a password reset destroys it and every message
   * ever encrypted to it, so the file has to exist BEFORE the migration, which
   * is the one point at which no client-side vault exists to make it from.
   *
   * It goes through export-legacy rather than any new endpoint: that call
   * already hands this browser the armored key after a fresh password, which is
   * exactly what the migration flow above does with it, and it refuses once the
   * account is client-protected. Nothing here widens what the server will give
   * out. The key is wrapped under a one-time secret before it touches disk —
   * a bare .asc download is deliberately not offered, because an unprotected
   * private key in a downloads folder is the failure this whole page exists to
   * prevent.
   */
  async function handleDownloadLegacyBackup(e: FormEvent) {
    e.preventDefault();
    if (!pgpIdentity) return;
    setPgpBusy(true);
    setPgpStatus("");
    try {
      const exported = await exportLegacyPGPKey(legacyBackupPassword);
      const imported = await importIdentity(exported.privateKey, "");
      const { backup, secret } = await createRecoveryBackup(
        imported.armoredPrivateKey, { fingerprint: imported.fingerprint, publicKey: imported.armoredPublicKey }
      );
      if (!mounted.current) return;
      saveRecoveryBackup(backup, backup.fingerprint, secret, requirePGPRevision(exported));
      setLegacyBackupOpen(false);
      setLegacyBackupPassword("");
    } catch (err) {
      setPgpStatus(`Backup failed: ${toErrorMessage(err, "check your password and try again")}`);
    } finally {
      setPgpBusy(false);
    }
  }

  function handleRestoreFileSelected(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    e.target.value = "";
    if (!file) return;
    setRestoreSource({ kind: "file", file });
    setDrillDate("");
    setRestoreSecret("");
    setRestorePassword("");
    setPgpStatus("");
  }

  function cancelRestore() {
    setRestoreSource(null);
    setDrillDate("");
    setRestoreSecret("");
    setRestorePassword("");
  }

  async function openServerRecovery() {
    setPgpBusy(true);
    setPgpStatus("");
    cancelRestore();
    try {
      const backup = await getRecoveryBackup();
      setRestoreSource({ kind: "server" });
      try {
        setDrillDate(await lastRecoveryDrill(userId ?? "", backup));
      } catch {
        setPgpStatus("Recovery copy loaded. This browser could not read its drill date.");
      }
    } catch (err) {
      setPgpStatus(`Recovery copy unavailable: ${toErrorMessage(err, "try your downloaded file")}`);
    } finally {
      setPgpBusy(false);
    }
  }

  async function handleRestoreSubmit(e: FormEvent, drill = false) {
    e.preventDefault();
    if (!restoreSource) return;
    setPgpBusy(true);
    setPgpStatus("");
    setDrillDate("");
    try {
      // Read server bytes again: a drill must test the copy currently offered.
      if (restoreSource.kind === "file" && restoreSource.file.size > (512 << 10)) {
        throw new Error("The recovery backup is too large.");
      }
      const raw = restoreSource.kind === "file" ? await restoreSource.file.text() : JSON.stringify(await getRecoveryBackup());
      const restored = await restoreRecoveryBackup(raw, restoreSecret);
      // KDF/OpenPGP parsing may take time; compare against a fresh snapshot afterward.
      const current = await getPGPBootstrap();
      if (restored.format === "kypost-pgp-recovery-v2" || current.keyring != null) {
        if (restored.format !== "kypost-pgp-recovery-v2" || current.keyring == null || current.protection !== "client") {
          throw new Error("This backup is not the current complete keyring. A legacy backup cannot replace retained keys.");
        }
        await validateKeyringSnapshot(restored.privateKey, { ...current, keyring: current.keyring });
        if (!drill) {
          if (!mounted.current) return;
          await restorePGPKeyring(restored.privateKey, restorePassword, current, () => mounted.current);
          if (!mounted.current) return;
          cancelRestore();
          await loadPGPSession();
          setPgpStatus("Complete keyring restored and confirmed under your current account password. Every retained key was preserved.");
          return;
        }
        const backup: RecoveryBackup = { format: restored.format, fingerprint: restored.fingerprint,
          publicKey: restored.publicKey, envelope: restored.envelope, keyring: restored.keyring };
        setRestoreSecret("");
        setRestorePassword("");
        try {
          setDrillDate(await recordRecoveryDrill(userId ?? "", backup));
          setPgpStatus("Recovery drill passed. Every retained key and the current public identity match; the date was saved in this browser. No keys were changed.");
        } catch { setPgpStatus("Recovery drill passed, but this browser could not save the date. No keys were changed."); }
        return;
      }
      const imported = await importIdentity(restored.privateKey, "");
      const expected = current.fingerprint.toUpperCase();
      if (current.protection !== "client" || !expected || imported.fingerprint.toUpperCase() !== expected ||
          restored.fingerprint.toUpperCase() !== expected) {
        throw new Error("This backup belongs to a different PGP identity. Reload to see the current identity.");
      }
      if (drill) {
        // Only the sealed backup reaches the date helper; never its private key or secret.
        const backup: RecoveryBackup = { format: restored.format, fingerprint: restored.fingerprint, publicKey: restored.publicKey, envelope: restored.envelope };
        setRestoreSecret("");
        setRestorePassword("");
        try {
          setDrillDate(await recordRecoveryDrill(userId ?? "", backup));
          setPgpStatus("Recovery drill passed. The key opened and matches your identity; the date was saved in this browser.");
        } catch {
          setPgpStatus("Recovery drill passed, but this browser could not save the date.");
        }
        return;
      }
      const expectedRevision = requirePGPRevision(current);
      const wrapped = await wrapPrivateKey(imported.armoredPrivateKey, restorePassword);
      const committed = await rewrapPGPPrivateKey(JSON.stringify(wrapped), restorePassword, expected, expectedRevision);
      if (mounted.current) acceptCommittedPGPKey(imported.armoredPrivateKey, { fingerprint: expected, pgpRevision: committed.pgpRevision });
      cancelRestore();
      await loadPGPSession();
      setPgpStatus("PGP key restored and re-encrypted with your current account password.");
    } catch (err) {
      setPgpStatus(`${drill ? "Drill" : "Restore"} failed: ${toErrorMessage(err, "check the copy and recovery secret")}`);
    } finally {
      setPgpBusy(false);
    }
  }

  async function handleImportPGPIdentity(e: FormEvent) {
    e.preventDefault();
    setPgpBusy(true);
    setPgpStatus("");
    try {
      const password = window.prompt(
        "Enter your account password.\n\nThe imported key will be encrypted with it before it leaves this browser."
      );
      if (!password) {
        return;
      }
      // The key's own passphrase (if any) only unlocks it for the import; it
      // is then rewrapped under the account password, so the user has one
      // secret to remember rather than two.
      const expectedRevision = requirePGPRevision(pgpSession?.bootstrap);
      const imported = await importIdentity(pgpImportKey, pgpImportPassphrase);
      const wrapped = await wrapPrivateKey(imported.armoredPrivateKey, password);
      const id = await storeClientPGPIdentity(
        imported.armoredPublicKey,
        JSON.stringify(wrapped),
        "imported",
        password,
        expectedRevision
      );
      acceptCommittedPGPKey(imported.armoredPrivateKey, id);
      setPgpIdentity(id);
      setPgpImportOpen(false);
      setPgpImportKey("");
      setPgpImportPassphrase("");
      await loadPGPSession();
      setPgpStatus("PGP identity imported and encrypted with your account password.");
    } catch (e) {
      setPgpStatus(`Failed to import identity: ${toErrorMessage(e, "unknown error")}`);
    } finally {
      setPgpBusy(false);
    }
  }

  async function handleDeletePGPIdentity() {
    if (!window.confirm(
      "Delete your PGP identity? This also deletes the server recovery copy. Keep a downloaded recovery file and its secret before continuing, or mail encrypted to this key may be permanently unreadable." +
      (pgpSession?.bootstrap?.envelopeSlots?.includes("recovery") ? "" : "\n\nNo server recovery copy is confirmed for this identity.")
    )) {
      return;
    }
    // The account password, not just the confirmation. This is the one action
    // on this page that cannot be undone by any later one — a stolen session
    // could otherwise make every message ever encrypted to this key unreadable,
    // permanently, in a single request.
    const password = window.prompt(
      "Enter your account password to confirm deleting your PGP identity.\n\n" +
        "This cannot be undone: mail already encrypted to this key stays unreadable."
    );
    if (!password) {
      return;
    }
    setPgpBusy(true);
    setPgpStatus("");
    try {
      await deletePGPIdentity(password, requirePGPRevision(pgpIdentity));
      lockPGPSession();
      setPgpIdentity(null);
      await loadPGPSession();
      setPgpStatus("PGP identity deleted.");
    } catch (e) {
      setPgpStatus(`Failed to delete identity: ${toErrorMessage(e, "unknown error")}`);
    } finally {
      setPgpBusy(false);
    }
  }

  // Four states, not two: an identity whose custody is still loading must not be
  // reported as server-held, because that is the alarming answer.
  const keyCustody: "none" | "client" | "server" | "unknown" = !pgpIdentity
    ? pgpLoading
      ? "unknown"
      : "none"
    : !pgpSession?.bootstrap
      ? "unknown"
      : pgpSession.bootstrap.protection === "client"
        ? "client"
        : "server";

  return (
    <>
      <div
        className={`sec-card ${
          keyCustody === "client" ? "sec-card-on" : keyCustody === "server" ? "sec-card-risk" : ""
        }`}
      >
        <div className="sec-card-head">
          <p className="sec-eyebrow">Encryption</p>
          <h3>Email encryption (PGP)</h3>
        </div>
        {pgpLoading ? (
          <p className="sec-muted">Loading...</p>
        ) : pgpIdentity ? (
          <>
            <p className="sec-fingerprint">
              <span className="sec-muted-inline">fingerprint</span> {pgpIdentity.fingerprint}
              <br />
              <span className="sec-muted-inline">source</span> {pgpIdentity.source}
            </p>

            {pgpSession?.bootstrap?.protection === "client" ? (
              <div className="sec-section">
                <p className="sec-verdict sec-verdict-ok">
                  <span className="sec-pip sec-pip-on" aria-hidden="true" />
                  <span>End-to-end. This server cannot read your encrypted mail.</span>
                </p>
                <p className="sec-muted">
                  Your private key is encrypted with your account password and unlocked only in this browser tab.{" "}
                  {pgpSession.unlocked ? "It is unlocked for this session." : "It is locked — you will be asked for your password when you open or send encrypted mail."}
                </p>
                <p className="sec-muted">
                  <strong>Keep a backup of your key.</strong> Because the server cannot open it, an admin password
                  reset can leave it unreadable unless you have a recovery copy and its secret.
                </p>
                <div className="sec-actions">
                  {pgpSession.unlocked ? (
                    <button type="button" className="sec-action-quiet" onClick={() => lockPGPSession()}>
                      Lock key
                    </button>
                  ) : (
                    <button type="button" onClick={() => setUnlockOpen(true)}>
                      Unlock key
                    </button>
                  )}
                  <button
                    type="button"
                    className="sec-action-quiet"
                    onClick={() => setRecoverOpen((v) => !v)}
                  >
                    Key won&apos;t unlock?
                  </button>
                </div>
                {recoverOpen ? (
                  <form
                    onSubmit={(e) => void handleRecoverStaleEnvelope(e)}
                    className="auth-form sec-inline-form"
                  >
                    <h4>Re-encrypt your key</h4>
                    <p className="sec-muted">
                      If your key stopped opening with your current password, a past password change saved only
                      half-way. Enter the password your key was last encrypted under, plus your current one, and it
                      will be re-encrypted to match.
                    </p>
                    <label>
                      <div>Password your key was last encrypted under</div>
                      <input
                        type="password"
                        autoComplete="off"
                        value={recoverOldPassword}
                        onChange={(e) => setRecoverOldPassword(e.target.value)}
                      />
                    </label>
                    <label>
                      <div>Your current account password</div>
                      <input
                        type="password"
                        autoComplete="current-password"
                        value={recoverCurrentPassword}
                        onChange={(e) => setRecoverCurrentPassword(e.target.value)}
                      />
                    </label>
                    <div className="sec-actions">
                      <button
                        type="submit"
                        disabled={pgpBusy || recoverOldPassword === "" || recoverCurrentPassword === ""}
                      >
                        Re-encrypt key
                      </button>
                      <button
                        type="button"
                        className="sec-action-quiet"
                        onClick={() => setRecoverOpen(false)}
                      >
                        Cancel
                      </button>
                    </div>
                  </form>
                ) : null}
                <div className="sec-actions">
                  <button
                    type="button"
                    className="sec-action-primary"
                    disabled={pgpBusy || !pgpSession.unlocked || !!recoverySecret}
                    onClick={() => void handleDownloadRecoveryBackup()}
                  >
                    Download recovery backup
                  </button>
                  <label className="sec-action-file sec-action-quiet">
                    Restore recovery backup
                    <input
                      type="file"
                      accept="application/json,.json"
                      hidden
                      disabled={pgpBusy}
                      onChange={(e) => handleRestoreFileSelected(e)}
                    />
                  </label>
                </div>
                <p className="sec-muted">
                  {pgpSession.bootstrap.envelopeSlots?.includes("recovery")
                    ? "A server recovery copy is stored. You still need its secret; keep a downloaded file for server loss or identity deletion."
                    : "No server recovery copy is confirmed. Keep an offline file and secret before changing your password or deleting this identity."}
                </p>
                <button type="button" disabled={pgpBusy} onClick={() => void openServerRecovery()}>
                  Use server recovery copy
                </button>
                {restoreSource ? (
                  <form className="sec-inline-form" onSubmit={(e) => void handleRestoreSubmit(e)}>
                    <h4>{restoreSource.kind === "file" ? `Recovery file: ${restoreSource.file.name}` : "Server recovery copy"}</h4>
                    <p className="sec-muted">Test the secret with a drill, or restore the key under your current account password.</p>
                    {drillDate ? <p>Last successful drill of this copy in this browser: {new Date(drillDate).toLocaleString()}</p> : null}
                    <label className="sec-label">
                      Recovery secret
                      <input
                        type="password"
                        className="sec-input"
                        value={restoreSecret}
                        onChange={(e) => setRestoreSecret(e.target.value)}
                        autoComplete="off"
                        required
                      />
                    </label>
                    <label className="sec-label">
                      Current account password (restore only)
                      <input
                        type="password"
                        className="sec-input"
                        value={restorePassword}
                        onChange={(e) => setRestorePassword(e.target.value)}
                        autoComplete="current-password"
                      />
                    </label>
                    <div className="sec-actions">
                      <button type="submit" disabled={pgpBusy || !restoreSecret || !restorePassword}>Restore</button>
                      <button type="button" disabled={pgpBusy || !restoreSecret} onClick={(e) => void handleRestoreSubmit(e, true)}>Run recovery drill</button>
                      <button type="button" className="sec-action-quiet" disabled={pgpBusy} onClick={cancelRestore}>
                        Cancel
                      </button>
                    </div>
                  </form>
                ) : null}
              </div>
            ) : pgpSession?.bootstrap?.migrationAvailable ? (
              <div className="sec-section">
                <p className="sec-verdict sec-verdict-risk">
                  <span className="sec-pip sec-pip-risk" aria-hidden="true" />
                  <span>This server can read your encrypted mail.</span>
                </p>
                <p className="sec-muted">
                  Your private key is stored on this server, encrypted with a key kept on the same machine. Anyone
                  with access to the server or its backups can decrypt everything you have received. Migrating moves
                  the key under your account password so only your browser can open it.
                </p>
                <p className="sec-muted">
                  After migrating, an admin password reset will make the key unrecoverable — export a backup first.
                </p>
                {legacyBackupOpen ? (
                  <form
                    onSubmit={(e) => void handleDownloadLegacyBackup(e)}
                    className="auth-form sec-inline-form"
                  >
                    <h4>Confirm your password</h4>
                    <p className="sec-muted">
                      The key is wrapped in this browser under a one-time secret shown after the download, so the
                      file is safe to keep in a password manager or a cloud drive.
                    </p>
                    <label>
                      <div>Account password</div>
                      <input
                        type="password"
                        autoComplete="current-password"
                        value={legacyBackupPassword}
                        onChange={(e) => setLegacyBackupPassword(e.target.value)}
                        required
                      />
                    </label>
                    <div className="sec-actions">
                      <button type="submit" disabled={pgpBusy || legacyBackupPassword.length === 0}>
                        {pgpBusy ? "Preparing…" : "Download backup"}
                      </button>
                      <button
                        type="button"
                        className="sec-action-quiet"
                        onClick={() => {
                          setLegacyBackupOpen(false);
                          setLegacyBackupPassword("");
                        }}
                        disabled={pgpBusy}
                      >
                        Cancel
                      </button>
                    </div>
                  </form>
                ) : (
                  <div className="sec-actions">
                    <button
                      type="button"
                      className="sec-action-primary"
                      onClick={() => setLegacyBackupOpen(true)}
                      disabled={pgpBusy}
                    >
                      Download recovery backup
                    </button>
                  </div>
                )}
                {migrateOpen ? (
                  <form
                    onSubmit={(e) => void handleMigrateToClientProtection(e)}
                    className="auth-form sec-inline-form"
                  >
                    <h4>Confirm your password</h4>
                    <label>
                      <div>Account password</div>
                      <input
                        type="password"
                        autoComplete="current-password"
                        value={migratePassword}
                        onChange={(e) => setMigratePassword(e.target.value)}
                        required
                      />
                    </label>
                    <div className="sec-actions">
                      <button type="submit" disabled={pgpBusy || migratePassword.length === 0}>
                        {pgpBusy ? "Migrating…" : "Migrate to end-to-end"}
                      </button>
                      <button
                        type="button"
                        className="sec-action-quiet"
                        onClick={() => {
                          setMigrateOpen(false);
                          setMigratePassword("");
                        }}
                        disabled={pgpBusy}
                      >
                        Cancel
                      </button>
                    </div>
                  </form>
                ) : (
                  <div className="sec-actions">
                    <button type="button" onClick={() => setMigrateOpen(true)} disabled={pgpBusy}>
                      Migrate to end-to-end
                    </button>
                  </div>
                )}
              </div>
            ) : null}
            {/*
              Outside the custody branches on purpose: both of them produce a
              backup, and both owe the user the secret that opens it. A copy
              per branch is a copy that gets fixed in one place.
            */}
            {recoverySecret ? (
              <div className="sec-inline-form">
                <h4>Store this recovery secret</h4>
                <p className="sec-muted">
                  The recovery copy is useless without this secret. KyPost does not store the secret. Keep it separately from the file. Anyone with both can decrypt your historical mail.
                </p>
                <p className="sec-fingerprint"><code>{recoverySecret}</code></p>
                <div className="sec-actions">
                  <button
                    type="button"
                    className="sec-action-quiet"
                    onClick={async () => {
                      try {
                        await navigator.clipboard.writeText(recoverySecret);
                        setPgpStatus("Recovery secret copied.");
                      } catch {
                        setPgpStatus("Copy failed — select and copy the secret manually.");
                      }
                    }}
                  >
                    Copy secret
                  </button>
                  {recoveryBackup?.format === "kypost-pgp-recovery-v1" && pgpSession?.bootstrap?.protection === "client" ? (
                    <button type="button" disabled={pgpBusy} onClick={() => void handleStoreRecoveryBackup()}>
                      I saved the secret — store server copy
                    </button>
                  ) : null}
                  {recoveryBackup && preparedRecovery ? <button type="button" onClick={() => {
                    try { saveRecoveryBackup(recoveryBackup, recoveryBackup.fingerprint, recoverySecret, preparedRecovery.expectedRevision); }
                    catch (err) { setPgpStatus(`Download failed: ${toErrorMessage(err, "try again")}`); }
                  }}>Download file again</button> : null}
                  <button type="button" disabled={pgpBusy} onClick={() => { setRecoverySecret(""); setRecoveryBackup(null); }}>I saved the file and secret</button>
                </div>
              </div>
            ) : null}
            <p className="sec-muted">
              {selfContact ? (
                <>Sharing contact card: {selfContact.fn} · <Link to="/contacts">Manage in Contacts</Link></>
              ) : (
                <>No contact card set — <Link to="/contacts">add one in Contacts</Link> and mark it as yours to include it when sharing your PGP key.</>
              )}
            </p>
            <details className="sec-details">
              <summary>Show public key</summary>
              <pre className="sec-pubkey">{pgpIdentity.publicKey}</pre>
            </details>
            <div className="sec-actions">
              <button
                type="button"
                className="sec-action-danger"
                onClick={() => void handleDeletePGPIdentity()}
                disabled={pgpBusy}
              >
                Delete identity
              </button>
            </div>
          </>
        ) : (
          <>
            {/*
              The mobile question used to be asked here, and "yes" minted a key this
              server held. Server custody is retired, so there is no longer a choice to
              present: the key is generated in this browser and the server never sees it.
              Reading on other devices is served by per-device envelope slots, which seal
              the key to that device rather than to this server.
            */}
            <div className="sec-actions">
              <button
                type="button"
                onClick={() => void handleGeneratePGPIdentity()}
                disabled={pgpBusy}
              >
                Generate new identity
              </button>
              <button
                type="button"
                className="sec-action-quiet"
                onClick={() => setPgpImportOpen(!pgpImportOpen)}
                disabled={pgpBusy}
              >
                Import existing key
              </button>
            </div>
            {pgpImportOpen ? (
              <form
                onSubmit={(e) => void handleImportPGPIdentity(e)}
                className="auth-form sec-inline-form"
              >
                <h4>Import a key</h4>
                <label>
                  <div>Armored private key</div>
                  <textarea
                    value={pgpImportKey}
                    onChange={(e) => setPgpImportKey(e.target.value)}
                    rows={4}
                    placeholder="-----BEGIN PGP PRIVATE KEY BLOCK-----"
                    required
                  />
                </label>
                <label>
                  <div>Passphrase (leave blank if none)</div>
                  <input
                    type="password"
                    value={pgpImportPassphrase}
                    onChange={(e) => setPgpImportPassphrase(e.target.value)}
                  />
                </label>
                <div className="sec-actions">
                  <button type="submit" disabled={pgpBusy}>Import</button>
                </div>
              </form>
            ) : null}
          </>
        )}
        {pgpStatus ? <p className="sec-muted">{pgpStatus}</p> : null}

        {discoverySettings ? (
          <div className="sec-subsection">
            <h5>Key discovery</h5>
            <label className="sec-check">
              <input
                type="checkbox"
                checked={discoverySettings.autoEncryptWhenKeyKnown}
                disabled={discoveryBusy}
                onChange={(e) => void updateDiscoverySetting({ autoEncryptWhenKeyKnown: e.target.checked })}
              />
              Encrypt automatically when I have a recipient's key
            </label>
            <label className="sec-check">
              <input
                type="checkbox"
                checked={discoverySettings.storeDiscoveredKeys}
                disabled={discoveryBusy}
                onChange={(e) => void updateDiscoverySetting({ storeDiscoveredKeys: e.target.checked })}
              />
              Save keys I discover to my contacts
            </label>
            <label className="sec-check">
              <input
                type="checkbox"
                checked={discoverySettings.advertiseAutocrypt}
                disabled={discoveryBusy}
                onChange={(e) => void updateDiscoverySetting({ advertiseAutocrypt: e.target.checked })}
              />
              Advertise my public key on outgoing mail (Autocrypt)
            </label>
            <p className="sec-check-note">
              Adds an Autocrypt header so people you email can automatically discover your key. On by
              default.
            </p>
            <label className="sec-check">
              <input
                type="checkbox"
                checked={discoverySettings.publishWKD}
                disabled={discoveryBusy}
                onChange={(e) => void updateDiscoverySetting({ publishWKD: e.target.checked })}
              />
              Publish my public key via Web Key Directory (WKD)
            </label>
            <p className="sec-check-note">
              Lets people look up your key at your mail domain. Requires an administrator to have set up
              WKD for that domain. On by default.
            </p>
            {discoveryStatus ? <p className="sec-muted">{discoveryStatus}</p> : null}
            {suppressions.length > 0 ? (
              <div className="sec-subsection">
                <h5>Discovery opt-outs</h5>
                <ul className="sec-list">
                  {suppressions.map((s) => (
                    <li key={s.email}>
                      <span>
                        {s.email} <span className="sec-muted-inline">({s.reason})</span>
                      </span>
                      <button
                        type="button"
                        className="sec-action-quiet"
                        onClick={() => void allowDiscoveryAgain(s.email)}
                      >
                        Allow discovery again
                      </button>
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
          </div>
        ) : null}
      </div>
    </>
  );
}
