import { useState, type ChangeEvent } from "react";
import { credentialFields, deriveCredential } from "../../api/auth";
import { HttpError, postJSON, toErrorMessage } from "../../api/client";
import { withSSOStepUp } from "../../api/stepup";
import { visible } from "../../lib/visibleText";
import type { ImportStatus } from "./MailImport";

type RemoteFolder = { name: string; path: string[]; attributes: string[] };

// The only grant the server mints; anything else is not followed.
const grantToken = /^[0-9a-f]{64}$/;
// Gmail's All Mail and Starred hold every message again.
const repeats = (f: RemoteFolder) => f.attributes.includes("\\all") || f.attributes.includes("\\flagged");

/**
 * Import from another mail account over IMAP: confirm (account password or
 * KySignOn), sign in and list the folders, choose, import. The provider
 * password goes only to the folders request, never into the confirmed one.
 */
export function MailImportAccount({ mailbox, ssoSession, disabled, onStarted }: {
  mailbox: string;
  ssoSession: boolean;
  disabled: boolean;
  onStarted: (status: ImportStatus) => void;
}) {
  const [host, setHost] = useState("");
  const [security, setSecurity] = useState("tls");
  const [port, setPort] = useState("993");
  const [username, setUsername] = useState("");
  const [providerPassword, setProviderPassword] = useState("");
  const [accountPassword, setAccountPassword] = useState("");
  const [token, setToken] = useState("");
  const [folders, setFolders] = useState<RemoteFolder[] | null>(null);
  const [chosen, setChosen] = useState<Set<string>>(new Set());
  const [target, setTarget] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const reset = () => { setToken(""); setFolders(null); };
  // A different account needs a new confirmation.
  const edit = (set: (v: string) => void) => (e: ChangeEvent<HTMLInputElement | HTMLSelectElement>) => { set(e.target.value); reset(); };

  async function listFolders() {
    setBusy(true); setError("");
    const secret = providerPassword, kypostPassword = accountPassword;
    setProviderPassword(""); setAccountPassword("");
    try {
      let grant = token;
      if (!grant) {
        const credential = ssoSession ? {} : credentialFields(await deriveCredential("", kypostPassword));
        const body = { mailbox, host, port: Number(port), security, username, ...credential };
        const r = await withSSOStepUp(headers => postJSON<{ token: string }>("/api/import/imap", body, headers));
        if (!grantToken.test(r.token)) throw new Error("The server answered with an unexpected import link.");
        grant = r.token;
        setToken(grant);
      }
      const r = await postJSON<{ folders: RemoteFolder[]; target: string }>(`/api/import/imap/${grant}/folders`, { password: secret });
      setFolders(r.folders);
      setTarget(r.target);
      setChosen(new Set(r.folders.filter(f => !repeats(f)).map(f => f.name)));
    } catch (e: unknown) {
      if (e instanceof HttpError && e.status === 404) reset();
      setError(toErrorMessage(e, "Could not list the folders."));
    } finally {
      setBusy(false);
    }
  }

  async function start() {
    setBusy(true); setError("");
    try {
      onStarted(await postJSON<ImportStatus>(`/api/import/imap/${token}/start`, { folders: [...chosen], target }));
      reset();
    } catch (e: unknown) {
      if (e instanceof HttpError && e.status === 404) reset();
      setError(toErrorMessage(e, "Import failed."));
    } finally {
      setBusy(false);
    }
  }

  const toggle = (name: string) => setChosen(old => {
    const next = new Set(old);
    if (!next.delete(name)) next.add(name);
    return next;
  });
  const canList = host.trim() !== "" && username.trim() !== "" && providerPassword !== "" && (ssoSession || token !== "" || accountPassword !== "");
  return <>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    <p>KyPost's server signs in to your other mail provider and copies the folders you choose. The password is used only for this import and is never stored. Gmail, iCloud and Outlook usually need an app password, created in that account's security settings. Nothing is changed on the other account: folders are opened read-only, and messages keep their read and flagged state and their received date. Importing again skips the messages already there.</p>
    <fieldset className="config-card config-grid" disabled={busy || disabled}>
      <legend>Other mail account</legend>
      <label>Server<input aria-label="Server" placeholder="imap.example.com" autoComplete="off" value={host} onChange={edit(setHost)} /></label>
      <label>Security
        <select aria-label="Security" value={security} onChange={e => { edit(setSecurity)(e); setPort(e.target.value === "tls" ? "993" : "143"); }}>
          <option value="tls">TLS</option>
          <option value="starttls">STARTTLS</option>
        </select>
      </label>
      <label>Port (993 for TLS, 143 for STARTTLS)<input aria-label="Port" inputMode="numeric" value={port} onChange={edit(setPort)} /></label>
      <label>Username<input aria-label="Username" autoComplete="off" value={username} onChange={edit(setUsername)} /></label>
      <label>Password or app password<input aria-label="Provider password" type="password" autoComplete="new-password" value={providerPassword} onChange={e => setProviderPassword(e.target.value)} /></label>
      {!token && (ssoSession ? <p>You will confirm with KySignOn.</p> : <label>Account password<input type="password" autoComplete="current-password" value={accountPassword} onChange={e => setAccountPassword(e.target.value)} /></label>)}
      <button className="button" disabled={!mailbox || !canList} onClick={() => void listFolders()}>List folders</button>
    </fieldset>
    {folders && <fieldset className="config-card config-grid" disabled={busy || disabled}>
      <legend>Folders to import</legend>
      {folders.length === 0 && <p>The account has no folders that can be imported.</p>}
      {folders.map(f => <label key={f.name}>
        <input type="checkbox" checked={chosen.has(f.name)} onChange={() => toggle(f.name)} /> {visible(f.path.join(" / "))}
        {repeats(f) && " (repeats mail from your other folders; usually left out)"}
      </label>)}
      <label>Into folder<input aria-label="Into folder" value={target} onChange={e => setTarget(e.target.value)} /></label>
      <button className="button" disabled={chosen.size === 0} onClick={() => void start()}>Import folders</button>
    </fieldset>}
  </>;
}
