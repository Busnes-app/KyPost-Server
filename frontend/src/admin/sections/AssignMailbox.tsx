import { useEffect, useState } from "react";
import { toErrorMessage } from "../../api/client";
import {
  deleteUserCardDAVClient,
  deleteUserIMAPConfig,
  getUserCardDAVClient,
  getUserIMAPConfig,
  putUserCardDAVClient,
  putUserIMAPConfig,
  type ManagedUser,
  type UserCardDAVInput,
  type UserIMAPInput
} from "../../api/users";

const EMPTY_IMAP: UserIMAPInput = {
  host: "", port: 993, username: "", password: "", mailbox: "INBOX", smtpHost: "", smtpPort: 587, managed: false
};
const EMPTY_CARDDAV: UserCardDAVInput = { serverUrl: "", username: "", password: "", addressBookPath: "", managed: false };

/**
 * Admin editor for one user's mailbox and contacts-sync credentials. A blank
 * password keeps whatever is stored, so an admin can flip the lock without
 * knowing the secret. Lives inline under the user's row in Manage Users.
 */
export function AssignMailbox({ user, onClose }: { user: ManagedUser; onClose: () => void }) {
  const [imap, setImap] = useState<UserIMAPInput>(EMPTY_IMAP);
  const [imapConfigured, setImapConfigured] = useState(false);
  const [dav, setDav] = useState<UserCardDAVInput>(EMPTY_CARDDAV);
  const [davConfigured, setDavConfigured] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");

  async function refreshImap() {
    const i = await getUserIMAPConfig(user.id);
    setImapConfigured(i.configured);
    if (i.configured) {
      setImap({
        host: i.host ?? "", port: i.port ?? 993, username: i.username ?? "", password: "",
        mailbox: i.mailbox ?? "INBOX", smtpHost: i.smtpHost ?? "", smtpPort: i.smtpPort ?? 587, managed: i.managed === true
      });
    }
  }

  async function refreshDav() {
    const d = await getUserCardDAVClient(user.id);
    setDavConfigured(d.configured);
    if (d.configured) {
      setDav({
        serverUrl: d.serverUrl ?? "", username: d.username ?? "", password: "",
        addressBookPath: d.addressBookPath ?? "", managed: d.managed === true
      });
    }
  }

  useEffect(() => {
    void Promise.all([refreshImap(), refreshDav()]).catch((e: unknown) =>
      setMessage(`Failed to load: ${toErrorMessage(e, "unknown error")}`)
    );
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [user.id]);

  async function run(label: string, action: () => Promise<unknown>, after: () => Promise<void>) {
    setBusy(true);
    setMessage("");
    try {
      await action();
      await after();
      setMessage(label);
    } catch (e: unknown) {
      setMessage(`Failed: ${toErrorMessage(e, "unknown error")}`);
    } finally {
      setBusy(false);
    }
  }

  const tone = message.startsWith("Failed") ? "notice notice-error" : "notice notice-success";

  return (
    <div className="config-card">
      <h3>Mailbox for {user.username}</h3>
      <p className="config-muted">
        Leave the password blank to keep the stored one. Locking makes the user&apos;s tab read-only; they can still see
        the host and username.
      </p>

      <div className="config-grid config-grid-two">
        <label><div>IMAP Host</div><input value={imap.host} onChange={(e) => setImap((p) => ({ ...p, host: e.target.value }))} /></label>
        <label><div>IMAP Port</div><input type="number" value={imap.port} onChange={(e) => setImap((p) => ({ ...p, port: Number(e.target.value) || 993 }))} /></label>
        <label><div>IMAP Username</div><input value={imap.username} onChange={(e) => setImap((p) => ({ ...p, username: e.target.value }))} /></label>
        <label><div>IMAP Password</div><input type="password" value={imap.password} placeholder={imapConfigured ? "Blank keeps stored password" : "Required"} onChange={(e) => setImap((p) => ({ ...p, password: e.target.value }))} /></label>
        <label><div>Mailbox</div><input value={imap.mailbox} onChange={(e) => setImap((p) => ({ ...p, mailbox: e.target.value }))} /></label>
        <label><div>SMTP Host (optional)</div><input value={imap.smtpHost} onChange={(e) => setImap((p) => ({ ...p, smtpHost: e.target.value }))} /></label>
        <label><div>SMTP Port (optional)</div><input type="number" value={imap.smtpPort} onChange={(e) => setImap((p) => ({ ...p, smtpPort: Number(e.target.value) || 587 }))} /></label>
        <label className="config-checkbox">
          <input type="checkbox" checked={imap.managed} onChange={(e) => setImap((p) => ({ ...p, managed: e.target.checked }))} />
          <span>Lock mail settings (user cannot change them)</span>
        </label>
      </div>
      <div className="config-actions">
        <button type="button" disabled={busy} onClick={() => void run("Mailbox saved.", () => putUserIMAPConfig(user.id, imap), refreshImap)}>Save Mailbox</button>
        {imapConfigured ? (
          <button type="button" disabled={busy} onClick={() => {
            if (window.confirm(`Remove ${user.username}'s stored mailbox credentials?`)) {
              void run("Mailbox removed.", () => deleteUserIMAPConfig(user.id), refreshImap);
            }
          }}>Remove Mailbox</button>
        ) : null}
      </div>

      <h3>Contacts sync for {user.username}</h3>
      <div className="config-grid config-grid-two">
        <label><div>CardDAV Server URL</div><input value={dav.serverUrl} onChange={(e) => setDav((p) => ({ ...p, serverUrl: e.target.value }))} placeholder="https://contacts.example.com/dav/" /></label>
        <label><div>CardDAV Username</div><input value={dav.username} onChange={(e) => setDav((p) => ({ ...p, username: e.target.value }))} /></label>
        <label><div>CardDAV Password</div><input type="password" value={dav.password} placeholder={davConfigured ? "Blank keeps stored password" : "Required"} onChange={(e) => setDav((p) => ({ ...p, password: e.target.value }))} /></label>
        <label><div>Address Book Path (optional)</div><input value={dav.addressBookPath} onChange={(e) => setDav((p) => ({ ...p, addressBookPath: e.target.value }))} /></label>
        <label className="config-checkbox">
          <input type="checkbox" checked={dav.managed} onChange={(e) => setDav((p) => ({ ...p, managed: e.target.checked }))} />
          <span>Lock contacts sync settings</span>
        </label>
      </div>
      <div className="config-actions">
        <button type="button" disabled={busy} onClick={() => void run("Contacts sync saved.", () => putUserCardDAVClient(user.id, dav), refreshDav)}>Save Contacts Sync</button>
        {davConfigured ? (
          <button type="button" disabled={busy} onClick={() => {
            if (window.confirm(`Remove ${user.username}'s stored CardDAV client credentials?`)) {
              void run("Contacts sync removed.", () => deleteUserCardDAVClient(user.id), refreshDav);
            }
          }}>Remove Contacts Sync</button>
        ) : null}
        <button type="button" onClick={onClose}>Close</button>
      </div>
      {message ? <p className={tone}>{message}</p> : null}
    </div>
  );
}
