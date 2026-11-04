// Confirms a contact's Autocrypt key once the user has compared its
// fingerprint with the correspondent, so that key's signatures read as
// verified. It keeps its own password field, so ContactDetailsDialog stays
// stateless.

import { useState } from "react";
import { toErrorMessage } from "../../api/client";
import { confirmContactPGPKey, type Contact } from "../../api/contacts";
import { useAuth } from "../../auth";

export function ConfirmPGPKey({ contact, onConfirmed }: { contact: Contact; onConfirmed: () => void | Promise<void> }) {
  const auth = useAuth();
  const ssoSession = auth.ssoSession ?? false;
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const pinned = contact.pgpKeyFingerprint ?? "";

  async function confirm() {
    setBusy(true);
    setMessage("");
    try {
      await confirmContactPGPKey(contact.uid, pinned, { ssoSession, accountPassword: password });
      await onConfirmed();
    } catch (error: unknown) {
      setMessage(`Could not confirm the key: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setPassword("");
      setBusy(false);
    }
  }

  return (
    <div>
      <p className="contacts-muted">
        This key was taken from their mail and is not confirmed, so their signatures show as unconfirmed. Ask {contact.fn} for
        their key fingerprint, in person or by phone, and confirm only if it matches:
      </p>
      <p className="contacts-pgp-fingerprint">
        <code>{pinned.toUpperCase().replace(/(.{4})(?=.)/g, "$1 ")}</code>
      </p>
      {ssoSession ? null : (
        <label className="sec-label">
          <span>Account password to confirm</span>
          <input
            className="sec-input"
            type="password"
            autoComplete="current-password"
            value={password}
            disabled={busy}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
      )}
      <button type="button" onClick={() => void confirm()} disabled={busy || (!ssoSession && !password)}>
        {busy ? "Confirming…" : "Confirm key"}
      </button>
      {message ? <p className="contacts-pgp-error">{message}</p> : null}
    </div>
  );
}
