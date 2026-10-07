import { useEffect, useState } from "react";
import { useAuth } from "../../auth";
import { credentialFields, deriveCredential } from "../../api/auth";
import { getJSON, postJSON, toErrorMessage } from "../../api/client";
import { withSSOStepUp } from "../../api/stepup";

type Mailbox = { id: string; addresses: { address: string }[] };
type Format = "mbox" | "eml-zip";

// The only URL the server mints; anything else is not followed.
const exportURL = /^\/api\/export\/[0-9a-f]{64}$/;

/** Self-service download of the user's own native mailbox. */
export function MailExport() {
  const ssoSession = useAuth().ssoSession === true;
  const [mailboxes, setMailboxes] = useState<Mailbox[] | null>(null);
  const [mailbox, setMailbox] = useState("");
  const [folders, setFolders] = useState<string[]>([]);
  const [folder, setFolder] = useState("");
  const [format, setFormat] = useState<Format>("mbox");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");

  useEffect(() => {
    let live = true;
    getJSON<{ mailboxes: Mailbox[] }>("/api/mailboxes")
      .then(r => { if (live) { setMailboxes(r.mailboxes); setMailbox(r.mailboxes[0]?.id ?? ""); } })
      .catch((e: unknown) => { if (live) { setMailboxes([]); setError(toErrorMessage(e, "Could not read your mailboxes.")); } });
    return () => { live = false; };
  }, []);

  useEffect(() => {
    if (!mailbox) return;
    let live = true;
    setFolders([]);
    setFolder("");
    getJSON<{ folders: string[] }>("/api/export/folders", { "X-KyPost-Mailbox": mailbox })
      .then(r => { if (live) setFolders(r.folders); })
      .catch((e: unknown) => { if (live) setError(toErrorMessage(e, "Could not read your folders.")); });
    return () => { live = false; };
  }, [mailbox]);

  async function start() {
    setBusy(true); setError(""); setNotice("");
    const accountPassword = password;
    setPassword("");
    try {
      const credential = ssoSession ? {} : credentialFields(await deriveCredential("", accountPassword));
      const body = { mailbox, folder, format, ...credential };
      const { url } = await withSSOStepUp(headers => postJSON<{ url: string }>("/api/export", body, headers));
      if (!exportURL.test(url)) throw new Error("The server answered with an unexpected download link.");
      window.location.assign(url);
      setNotice("Your download has started. Keep the file somewhere safe.");
    } catch (e: unknown) {
      setError(toErrorMessage(e, "Export failed."));
    } finally {
      setBusy(false);
    }
  }

  if (mailboxes === null) return <div className="config-section"><h3>Export mail</h3><p>Loading…</p></div>;
  return <div className="config-section">
    <h3>Export mail</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {notice && <p className="notice" role="status">{notice}</p>}
    {mailboxes.length === 0 ? <p>Export is available for mailboxes KyPost hosts. If your mail is with another provider, use that provider's export.</p> : <>
      <p>Download a copy of your mail exactly as stored, to keep or to move to another mail app. Encrypted messages stay encrypted: you need your own key to read them. Flags and labels are not included.</p>
      <p className="notice notice-warning">The file holds your private mail. Store it somewhere safe and delete copies you no longer need.</p>
      <fieldset className="config-card config-grid" disabled={busy}>
        <legend>What to export</legend>
        {mailboxes.length > 1 && <label>Mailbox
          <select aria-label="Mailbox" value={mailbox} onChange={e => setMailbox(e.target.value)}>
            {mailboxes.map(m => <option key={m.id} value={m.id}>{m.addresses[0]?.address ?? m.id}</option>)}
          </select>
        </label>}
        <label>Folder
          <select aria-label="Folder" value={folder} onChange={e => setFolder(e.target.value)}>
            <option value="">All folders</option>
            {folders.map(f => <option key={f} value={f}>{f}</option>)}
          </select>
        </label>
        <label><input type="radio" name="export-format" checked={format === "mbox"} onChange={() => setFormat("mbox")} /> One mbox file (Thunderbird and most mail apps)</label>
        <label><input type="radio" name="export-format" checked={format === "eml-zip"} onChange={() => setFormat("eml-zip")} /> A zip of .eml files, one per message</label>
        {ssoSession ? <p>You will confirm with KySignOn.</p> : <label>Account password<input type="password" autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} /></label>}
        <button className="button" disabled={!mailbox || (!ssoSession && !password)} onClick={() => void start()}>Export mail</button>
      </fieldset>
    </>}
  </div>;
}
