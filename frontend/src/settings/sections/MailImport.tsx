import { useEffect, useRef, useState } from "react";
import { useAuth } from "../../auth";
import { credentialFields, deriveCredential } from "../../api/auth";
import { getJSON, postJSON, toErrorMessage, uploadWithProgress } from "../../api/client";
import { withSSOStepUp } from "../../api/stepup";
import { MailImportAccount } from "./MailImportAccount";

type Mailbox = { id: string; addresses: { address: string }[] };
export type ImportStatus = {
  state: "idle" | "uploading" | "running" | "finished" | "failed" | "cancelled";
  folder?: string;
  imported: number;
  duplicates: number;
  skipped: number;
  bytes: number;
  error?: string;
  maxBytes?: number;
  maxMessageBytes?: number;
  // An import from another account: its server, the folder in progress, folders done of total.
  host?: string;
  current?: string;
  foldersDone?: number;
  foldersTotal?: number;
};

// The only URL the server mints; anything else is not followed.
const importURL = /^\/api\/import\/[0-9a-f]{64}$/;
export const POLL_MS = 1000;
// Polling stops after this many failures in a row.
const POLL_FAILURES = 5;
const mib = (n: number) => `${Math.max(1, Math.round(n / (1 << 20)))} MiB`;
const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;

/** Self-service import from mbox and EML files, or from another account over IMAP, into the user's own native mailbox. */
export function MailImport() {
  const ssoSession = useAuth().ssoSession === true;
  const [mode, setMode] = useState<"file" | "account">("file");
  const [mailboxes, setMailboxes] = useState<Mailbox[] | null>(null);
  const [mailbox, setMailbox] = useState("");
  const [folders, setFolders] = useState<string[]>([]);
  const [folder, setFolder] = useState("Imported");
  const [file, setFile] = useState<File | null>(null);
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [progress, setProgress] = useState<{ loaded: number; total: number } | null>(null);
  const [status, setStatus] = useState<ImportStatus | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [lost, setLost] = useState(false);
  const upload = useRef<AbortController | null>(null);

  useEffect(() => {
    let live = true;
    getJSON<{ mailboxes: Mailbox[] }>("/api/mailboxes")
      .then(r => { if (live) { setMailboxes(r.mailboxes); setMailbox(r.mailboxes[0]?.id ?? ""); } })
      .catch((e: unknown) => { if (live) { setMailboxes([]); setError(toErrorMessage(e, "Could not read your mailboxes.")); } });
    getJSON<ImportStatus>("/api/import")
      .then(s => { if (live) setStatus(s); })
      .catch(() => { /* the form still works; the next import reports its own status */ });
    return () => { live = false; upload.current?.abort(); };
  }, []);

  useEffect(() => {
    if (!mailbox) return;
    let live = true;
    setFolders([]);
    getJSON<{ folders: string[] }>("/api/export/folders", { "X-KyPost-Mailbox": mailbox })
      .then(r => { if (live) setFolders(r.folders); })
      .catch(() => { /* a typed folder name still works */ });
    return () => { live = false; };
  }, [mailbox]);

  const running = status?.state === "running";
  useEffect(() => {
    if (!running || lost) return;
    let failures = 0;
    const timer = window.setInterval(() => {
      getJSON<ImportStatus>("/api/import")
        .then(s => { failures = 0; setStatus(s); })
        .catch(() => {
          if (++failures < POLL_FAILURES) return;
          setLost(true);
          setError("Lost contact with the server; the import may still be running. Reload this page to check on it.");
        });
    }, POLL_MS);
    return () => window.clearInterval(timer);
  }, [running, lost]);

  async function start() {
    if (!file) return;
    setBusy(true); setError(""); setNotice(""); setLost(false); setProgress(null);
    const accountPassword = password;
    setPassword("");
    try {
      const credential = ssoSession ? {} : credentialFields(await deriveCredential("", accountPassword));
      const body = { mailbox, folder, ...credential };
      const { url, maxBytes } = await withSSOStepUp(headers => postJSON<{ url: string; maxBytes: number }>("/api/import", body, headers));
      if (!importURL.test(url)) throw new Error("The server answered with an unexpected upload link.");
      if (Number.isSafeInteger(maxBytes) && file.size > maxBytes) throw new Error(`The file is larger than your mailbox's storage (${mib(maxBytes)}) and cannot fit.`);
      upload.current = new AbortController();
      setProgress({ loaded: 0, total: file.size });
      const started = await uploadWithProgress<ImportStatus>(url, file, (loaded, total) => setProgress({ loaded, total }), upload.current.signal);
      setStatus(old => ({ ...old, ...started }));
    } catch (e: unknown) {
      if (e instanceof DOMException && e.name === "AbortError") {
        setNotice("Upload cancelled. Nothing was imported.");
        setStatus(old => old && { ...old, state: "idle" });
      } else {
        setError(toErrorMessage(e, "Import failed."));
        getJSON<ImportStatus>("/api/import").then(setStatus).catch(() => undefined);
      }
    } finally {
      upload.current = null;
      setProgress(null);
      setBusy(false);
    }
  }

  async function cancel() {
    if (upload.current) { upload.current.abort(); return; }
    try {
      setStatus(await postJSON<ImportStatus>("/api/import/cancel", {}));
    } catch (e: unknown) {
      setError(toErrorMessage(e, "Could not cancel the import."));
    }
  }

  if (mailboxes === null) return <div className="config-section"><h3>Import mail</h3><p>Loading…</p></div>;
  const limits = status?.maxBytes && status.maxMessageBytes
    ? ` The file can be up to ${mib(status.maxBytes)} (your mailbox's storage); a message over ${mib(status.maxMessageBytes)} is skipped.`
    : "";
  const counts = status && `${plural(status.imported, "message", "messages")} imported, ${plural(status.duplicates, "duplicate", "duplicates")} skipped, ${status.skipped} could not be imported`;
  const source = status?.host ? ` from ${status.host}` : "";
  const folderProgress = status?.host && status.foldersTotal ? ` (folder ${Math.min((status.foldersDone ?? 0) + 1, status.foldersTotal)} of ${status.foldersTotal}${status.current ? `, ${status.current}` : ""})` : "";
  return <div className="config-section">
    <h3>Import mail</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {notice && <p className="notice" role="status">{notice}</p>}
    {mailboxes.length === 0 ? <p>Import is available for mailboxes KyPost hosts.</p> : <>
      <p className="notice notice-warning">Imported mail is not scanned for spam, not run through your rules, and sends no notifications. Only import mail you trust. Import is unavailable while incoming encryption is on, because imported mail would be stored unencrypted.</p>
      <fieldset className="config-card config-grid" disabled={busy || running}>
        <legend>Import from</legend>
        <label><input type="radio" name="import-mode" checked={mode === "file"} onChange={() => setMode("file")} /> From a file</label>
        <label><input type="radio" name="import-mode" checked={mode === "account"} onChange={() => setMode("account")} /> From another mail account</label>
        {mailboxes.length > 1 && <label>Mailbox
          <select aria-label="Mailbox" value={mailbox} onChange={e => setMailbox(e.target.value)}>
            {mailboxes.map(m => <option key={m.id} value={m.id}>{m.addresses[0]?.address ?? m.id}</option>)}
          </select>
        </label>}
      </fieldset>
      {mode === "account" ? <MailImportAccount mailbox={mailbox} ssoSession={ssoSession} disabled={running} onStarted={s => { setError(""); setNotice(""); setLost(false); setStatus(old => ({ ...old, ...s })); }} /> : <>
      <p>Bring mail from another app: an mbox file (Thunderbird, Apple Mail, Google Takeout, or a KyPost export), a single .eml message, or a zip of .eml files. Messages are stored as parsed from the file, marked read, in the folder you choose. Importing the same file again skips the messages already there.{limits}</p>
      <fieldset className="config-card config-grid" disabled={busy || running}>
        <legend>What to import</legend>
        <label>Folder (existing or new)
          <input aria-label="Folder" list="import-folders" value={folder} onChange={e => setFolder(e.target.value)} />
          <datalist id="import-folders">{folders.map(f => <option key={f} value={f} />)}</datalist>
        </label>
        {/* No accept filter: Thunderbird keeps each folder as an mbox file without an extension. */}
        <label>File (.mbox, .eml, .zip, or an mbox without an extension)
          <input aria-label="File" type="file" onChange={e => setFile(e.target.files?.[0] ?? null)} />
        </label>
        {ssoSession ? <p>You will confirm with KySignOn.</p> : <label>Account password<input type="password" autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} /></label>}
        <button className="button" disabled={!mailbox || !file || (!ssoSession && !password)} onClick={() => void start()}>Import mail</button>
      </fieldset>
      </>}
    </>}
    {progress && <p role="status">Uploading… <progress aria-label="Upload progress" max={progress.total || 1} value={progress.loaded} /> {Math.floor(progress.loaded * 100 / (progress.total || 1))}%</p>}
    {running && <p role="status">Importing{source} into {status?.folder}{folderProgress}: {counts}. <button className="button secondary" onClick={() => void cancel()}>Cancel import</button></p>}
    {progress && <button className="button secondary" onClick={() => void cancel()}>Cancel upload</button>}
    {status?.state === "finished" && <p className="notice" role="status">Import{source} finished into {status.folder}: {counts}.</p>}
    {status?.state === "cancelled" && <p className="notice" role="status">Import cancelled: {counts}.</p>}
    {status?.state === "failed" && <p className="notice notice-error" role="alert">Import failed: {status.error ?? "unknown error"} ({counts}).</p>}
  </div>;
}
