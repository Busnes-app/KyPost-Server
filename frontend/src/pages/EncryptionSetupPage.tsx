import { useEffect, useState } from "react";
import { Link, Navigate } from "react-router";
import { getPGPIdentity, type PGPIdentity } from "../api/pgp";
import { useAuth } from "../auth";
import { PgpUnlockDialog } from "../components/PgpUnlockDialog";
import { loadPGPSession, subscribePGPSession, type PGPSessionState } from "../lib/pgpSession";
import { dismissSetup } from "../lib/setupDismissal";
import { MailKeys, type PreparedRecoveryBackup } from "./security/sections/MailKeys";

/**
 * First sign-in encryption setup. It reuses Security → Encryption's key
 * creation, import, recovery and incoming-encryption controls; the server
 * decides when setup is complete and holds mail until then if it is required.
 */
export function EncryptionSetupPage({ onRefresh }: { onRefresh: () => Promise<void> }) {
  const { userId = "", encryptionSetup: setup } = useAuth();
  const [pgpIdentity, setPgpIdentity] = useState<PGPIdentity | null>(null);
  const [pgpLoading, setPgpLoading] = useState(true);
  const [pgpSession, setPgpSession] = useState<PGPSessionState | null>(null);
  const [unlockOpen, setUnlockOpen] = useState(false);
  // Lifted here, as on SecurityPage: the recovery secret is shown once.
  const [recoverySecret, setRecoverySecret] = useState("");
  const [recoveryBackup, setRecoveryBackup] = useState<PreparedRecoveryBackup | null>(null);
  const [leaving, setLeaving] = useState(false);
  const [checked, setChecked] = useState(false);

  useEffect(() => {
    let cancelled = false;
    getPGPIdentity()
      .then((id) => { if (!cancelled) setPgpIdentity(id); })
      .catch(() => { if (!cancelled) setPgpIdentity(null); })
      .finally(() => { if (!cancelled) setPgpLoading(false); });
    return () => { cancelled = true; };
  }, []);
  useEffect(() => subscribePGPSession(setPgpSession), []);
  useEffect(() => { void loadPGPSession(); }, []);

  if (!setup || leaving) {
    return <Navigate to="/read" replace />;
  }
  const canFinish = !setup.required && setup.hasKey && setup.hasRecovery;
  const incomingRequired = setup.incomingPolicy === "required";

  async function finish() {
    await onRefresh();
    setChecked(true);
  }
  if (checked && canFinish) {
    return <Navigate to="/read" replace />;
  }

  return (
    <section className="panel sec-page" aria-labelledby="encryption-setup-heading">
      <header className="sec-header">
        <h2 id="encryption-setup-heading">Set up email encryption</h2>
        <p>
          {setup.required
            ? "Your administrator requires an encryption key. Your mail opens once these steps are done."
            : "Protect your mail with your own encryption key. You can also do this later in Security → Encryption."}
        </p>
      </header>

      <ol className="sec-custody">
        <li>
          <p className="sec-eyebrow">1. Key {setup.hasKey ? "· done" : ""}</p>
          <p>Create a new key, or import one you already use (from Proton, mailbox.org or GnuPG). It is made and encrypted in this browser; the server never sees it unencrypted.</p>
        </li>
        <li>
          <p className="sec-eyebrow">2. Recovery {setup.hasRecovery ? "· done" : ""}</p>
          <p>Your key is locked with your password, so a forgotten password means unreadable mail. Download a recovery backup, keep its recovery code somewhere safe, then confirm you saved it to store the server copy. Setup cannot finish without it.</p>
        </li>
        <li>
          <p className="sec-eyebrow">3. Encrypt all incoming mail {incomingRequired ? "· required by your administrator" : ""}{setup.incomingEnabled ? " · on" : ""}</p>
          <ul>
            <li>Mail is encrypted to your key as soon as it arrives. It&apos;s stored encrypted, and only your devices can read it.</li>
            <li>Senders who don&apos;t use encryption send it to us in plaintext. We encrypt it on arrival; we can&apos;t stop the sending server from seeing it.</li>
            <li>Server search and body filter rules can only use headers once mail is encrypted; AI labels are applied as mail arrives, before it is encrypted.</li>
          </ul>
          <p>{incomingRequired ? "" : "You can change this later in Security → Encryption."}</p>
        </li>
        <li>
          <p className="sec-eyebrow">4. Publish</p>
          <p>Your public key is published for your addresses so others can find it and send you encrypted mail.</p>
        </li>
      </ol>

      <div className="sec-layout">
        <MailKeys
          pgpIdentity={pgpIdentity}
          setPgpIdentity={setPgpIdentity}
          pgpLoading={pgpLoading}
          pgpSession={pgpSession}
          setUnlockOpen={setUnlockOpen}
          recoveryBackup={recoveryBackup}
          setRecoveryBackup={setRecoveryBackup}
          recoverySecret={recoverySecret}
          setRecoverySecret={setRecoverySecret}
          incomingDefaultOn={setup.incomingPolicy !== "off"}
        />
      </div>

      {checked && !canFinish ? (
        <p className="notice notice-error" role="alert">
          {!setup.hasKey
            ? "Create or import a key first."
            : !setup.hasRecovery
              ? "Store a recovery copy first: download the backup, save its recovery code, then confirm."
              : "Turn on Encrypt incoming mail first; your administrator requires it."}
        </p>
      ) : null}
      {pgpSession?.bootstrap && (pgpSession.bootstrap.suggestedUserIDs ?? []).length === 0 ? (
        <p className="notice">Your key needs your email address. <Link to="/settings/mail">Set up your mail account</Link> first.</p>
      ) : null}

      <div className="actions">
        <button type="button" className="btn" onClick={() => void finish()}>Finish setup</button>
        {!setup.required ? (
          <button type="button" className="btn" disabled={!!recoverySecret} onClick={() => { dismissSetup(userId); setLeaving(true); }}>
            Not now
          </button>
        ) : null}
      </div>

      <PgpUnlockDialog
        open={unlockOpen}
        reason="to unlock your PGP key for this session"
        onUnlocked={() => setUnlockOpen(false)}
        onCancel={() => setUnlockOpen(false)}
      />
    </section>
  );
}
