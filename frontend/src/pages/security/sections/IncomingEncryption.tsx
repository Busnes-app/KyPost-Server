import { useEffect, useEffectEvent, useState, type FormEvent } from "react";
import { getIncomingEncryption, setIncomingEncryption, type IncomingEncryptionPreference, type PGPIdentity } from "../../../api/pgp";
import { useAuth } from "../../../auth";
import { toErrorMessage } from "../../../api/client";

export function IncomingEncryption({ identity, clientProtected, defaultOn = false, onSaved }: { identity: PGPIdentity | null; clientProtected: boolean; defaultOn?: boolean; onSaved?: () => void }) {
  const auth = useAuth();
  // The server asks for the password whenever the account has one, whichever
  // way this session signed in; an account without one confirms with KySignOn.
  const needsPassword = auth.hasPassword !== false;
  // The server refuses turning it off under a Required policy, too.
  const required = auth.encryptionSetup?.incomingPolicy === "required";
  const [preference, setPreference] = useState<IncomingEncryptionPreference | null>(null);
  const [enabled, setEnabled] = useState(false);
  const [acknowledged, setAcknowledged] = useState(false);
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");

  // Preselected once, from the policy at load; later policy reads do not reset a choice.
  const loaded = useEffectEvent((value: IncomingEncryptionPreference) => {
    setPreference(value);
    setEnabled(value.enabled || defaultOn || required);
  });
  useEffect(() => {
    let active = true;
    getIncomingEncryption().then((value) => {
      if (active) loaded(value);
    }).catch((error: unknown) => {
      if (active) setMessage(toErrorMessage(error, "Could not load incoming encryption preference"));
    });
    return () => { active = false; };
  }, []);

  async function save(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    setMessage("");
    try {
      await setIncomingEncryption(enabled, password, identity, acknowledged);
      setPassword("");
      setPreference(await getIncomingEncryption());
      setMessage(enabled ? "Incoming encryption enabled." : "Future incoming encryption disabled. Existing encrypted mail stays encrypted.");
      onSaved?.();
    } catch (error) {
      setMessage(toErrorMessage(error, "Could not save incoming encryption preference"));
    } finally { setBusy(false); }
  }

  return (
    <section className="sec-card" aria-labelledby="incoming-encryption-heading">
      <div className="sec-card-head">
        <p className="sec-eyebrow">Incoming mail</p>
        <h3 id="incoming-encryption-heading">Classify, then encrypt</h3>
      </div>
      <p className="sec-muted">New mail is sorted, then encrypted to your key as it arrives. After that, only your devices can read it, but your mail provider and KyPost still see each message before it is encrypted. Keep your recovery backup: without your key, this mail cannot be opened.</p>
      <details className="sec-details">
        <summary>Details</summary>
        <p className="sec-muted">KyPost can classify unread inbox mail it has not processed yet, then replace the original with a copy encrypted to your public key. Attachments and the subject are inside the encrypted copy. Your private key stays on your devices.</p>
        <p className="sec-muted">The provider and KyPost see the original before encryption. Routing headers and labels remain visible; provider backups and copies on other devices may retain plaintext. Plaintext body caching stays disabled after you enable this. Already encrypted mail cannot be classified by the server. Corrections to encrypted replacements do not train the sorter from their original plaintext.</p>
        <p className="sec-muted">Keep a private-key recovery backup: losing the key makes these messages unreadable. Disabling this setting stops future replacements and does not decrypt existing mail. Pending replacements continue safely. Mail that cannot fit an encrypted copy is left plaintext and reported in Decisions after bounded retries. The configured polling mailbox must be INBOX. IMAP must support targeted deletion (UIDPLUS or IMAP4rev2).</p>
      </details>
      {!clientProtected || !identity ? <p className="sec-muted">Create your key above first.</p> : null}
      {preference?.pending ? <p role="status">An incoming replacement is pending. If it remains pending, check server health and restore the original PGP key or IMAP configuration.</p> : null}
      {required && preference?.enabled ? <p className="sec-check" role="status">Encrypt incoming mail after classification: on. Required by your administrator.</p> : <form onSubmit={(event) => { void save(event); }}>
        <label className="sec-check">
          <input type="checkbox" checked={enabled} disabled={busy || !preference || (!enabled && (!identity || !clientProtected))} onChange={(event) => { setEnabled(event.target.checked); setAcknowledged(false); }} />
          Encrypt incoming mail after classification
        </label>
        {enabled ? <label className="sec-check">
          <input type="checkbox" checked={acknowledged} disabled={busy} onChange={(event) => setAcknowledged(event.target.checked)} />
          I saved a private-key recovery backup and understand that plaintext originals will be removed.
        </label> : null}
        {needsPassword ? <label className="sec-label">
          <span>Account password</span>
          <input className="sec-input" type="password" autoComplete="current-password" value={password} disabled={busy} onChange={(event) => setPassword(event.target.value)} required />
        </label> : <p className="sec-muted">Confirm this change through KySignOn when prompted.</p>}
        <button type="submit" className="btn" disabled={busy || !preference || (needsPassword && !password) || (enabled && !acknowledged) || (required && !enabled)}>{busy ? "Saving…" : "Save incoming encryption"}</button>
        {required ? <p className="sec-muted">Required by your administrator.</p> : null}
      </form>}
      {message ? <p role="status">{message}</p> : null}
    </section>
  );
}
