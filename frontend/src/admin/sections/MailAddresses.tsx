import { useEffect, useRef, useState } from "react";
import { useAuth } from "../../auth";
import { credentialFields, deriveCredential } from "../../api/auth";
import { deleteJSON, getJSON, HttpError, postJSON, toErrorMessage } from "../../api/client";
import { readMailAddresses, readMailAddressChange, type NativeMailbox } from "../../api/nativeMail";
import { listUsers } from "../../api/users";
import { withSSOStepUp } from "../../api/stepup";

type Change =
  | { action: "add"; address: string; mailbox: string }
  | { action: "release"; address: string }
  | { action: "reassign"; address: string; mailbox: string };

export function MailAddresses() {
  const auth = useAuth();
  if (!auth.authenticated || auth.role !== "admin") return null;
  return <MailAddressesForm key={`${auth.userId}:${auth.username}:${auth.ssoSession}`} />;
}

function MailAddressesForm() {
  const ssoSession = useAuth().ssoSession === true;
  const live = useRef(false);
  const refreshGeneration = useRef(0);
  const inFlight = useRef(false);
  const [mailboxes, setMailboxes] = useState<NativeMailbox[] | null>(null);
  const [names, setNames] = useState(new Map<string, string>());
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [warning, setWarning] = useState("");
  const [busy, setBusy] = useState(false);
  const [password, setPassword] = useState("");
  const [owner, setOwner] = useState("");
  const [addTo, setAddTo] = useState("");
  const [address, setAddress] = useState("");
  const [targets, setTargets] = useState<Record<string, string>>({});
  function requireLive() {
    if (!live.current) throw new Error("Mail addresses closed; reopen them to continue.");
  }
  async function refresh() {
    const generation = ++refreshGeneration.current;
    const [list, users] = await Promise.all([getJSON<unknown>("/api/admin/mail-addresses"), listUsers()]);
    const next = readMailAddresses(list);
    if (!live.current || generation !== refreshGeneration.current) return;
    setNames(new Map(users.filter(u => typeof u.id === "string" && typeof u.username === "string").map(u => [u.id, u.username])));
    setMailboxes(next);
  }
  useEffect(() => {
    live.current = true;
    const generation = refreshGeneration.current + 1;
    void refresh().catch((e: unknown) => {
      if (live.current && generation === refreshGeneration.current) setError(toErrorMessage(e, "Unable to load mail addresses. Reload before changing them."));
    });
    return () => { live.current = false; refreshGeneration.current++; };
  }, []);
  function label(id: string) {
    const m = mailboxes?.find(x => x.mailbox === id);
    const primary = m?.addresses.find(a => a.kind === "primary")?.address;
    const user = m && (names.get(m.user) ?? m.user);
    return `${primary ?? id}${user ? ` (${user})` : ""}`;
  }
  async function act(change: Change, confirmText = "") {
    if (!mailboxes || inFlight.current) return;
    if (confirmText && !window.confirm(confirmText)) return;
    inFlight.current = true;
    setBusy(true); setError(""); setNotice(""); setWarning("");
    // Freeze the request before credential derivation or KySignOn confirmation.
    const fields = change.action === "add" ? { mailbox: change.mailbox, address: change.address }
      : change.action === "reassign" ? { mailbox: change.mailbox } : {};
    const path = `/api/admin/mail-addresses/${encodeURIComponent(change.address)}`;
    const accountPassword = password;
    setPassword("");
    try {
      const credential = ssoSession ? {} : credentialFields(await deriveCredential("", accountPassword));
      requireLive();
      const body = { ...fields, ...credential };
      const result = await withSSOStepUp((headers) => {
        requireLive();
        return change.action === "add" ? postJSON<unknown>("/api/admin/mail-addresses", body, headers)
          : change.action === "release" ? deleteJSON<unknown>(path, body, headers)
          : postJSON<unknown>(`${path}/reassign`, body, headers);
      });
      requireLive();
      const { record, warning } = readMailAddressChange(result, change.address);
      if (change.action === "release" ? record.state !== "reserved" : record.mailbox !== change.mailbox) {
        throw new Error("Address change answer does not match the request; reload before retrying.");
      }
      // A committed change followed by an unreadable list must not leave stale controls enabled.
      setMailboxes(null);
      await refresh();
      requireLive();
      if (change.action === "add") setAddress("");
      setNotice(change.action === "release" ? `${record.address} released and reserved (generation ${record.generation}). It receives and sends nothing until reassigned.`
        : `${record.address} is ${record.state} on ${label(record.mailbox)} (generation ${record.generation}).`);
      setWarning(warning);
    } catch (e: unknown) {
      if (live.current) {
        setMailboxes(null);
        setError(toErrorMessage(e, "Address change failed. Reload before retrying an uncertain change."));
        // The server answered, so controls return only if a fresh list validates.
        // A request that never got an answer stays locked until reload.
        if (e instanceof HttpError) await refresh().catch(() => undefined);
      }
    } finally {
      inFlight.current = false;
      if (live.current) { setPassword(""); setBusy(false); }
    }
  }
  const unlocked = ssoSession || password.length > 0;
  const owners = [...new Set(mailboxes?.map(m => m.user).filter(Boolean))];
  const shown = mailboxes?.filter(m => !owner || m.user === owner) ?? [];
  const typed = address.trim();
  return <div className="config-section">
    <h3>Mail addresses</h3>
    <p>Native mailboxes and the addresses that deliver to them. Primary addresses come from KyIdentity and change only there. Aliases belong to everyday identities; administrator identities own none.</p>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {notice && <p className="notice" role="status">{notice}</p>}
    {warning && <p className="notice notice-warning" role="status">Change saved with a warning: {warning}</p>}
    {!mailboxes && <p>{error ? "Address list unavailable. Reload this page before making changes." : "Loading mail addresses…"}</p>}
    <fieldset className="config-card config-grid" disabled={busy || !mailboxes}>
      <legend>Confirm each action</legend>
      {ssoSession ? <p>Confirm each action with KySignOn.</p> : <label>Account password<input type="password" autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} /></label>}
      <label>Show mailboxes of<select value={owner} onChange={e => setOwner(e.target.value)}>
        <option value="">All users</option>
        {owners.map(id => <option key={id} value={id}>{names.get(id) ?? id}</option>)}
      </select></label>
      {mailboxes?.length === 0 && <p>No native mailboxes yet. KyPost creates one when KyIdentity assigns an everyday identity a primary address.</p>}
      {shown.map(m => <div key={m.mailbox} className="users-table-wrap"><table className="users-table">
        <caption>{label(m.mailbox)}</caption>
        <thead><tr><th scope="col">Address</th><th scope="col">Kind</th><th scope="col">State</th><th scope="col">Generation</th><th scope="col">Actions</th></tr></thead>
        <tbody>{m.addresses.map(a => <tr key={a.address}>
          <td>{a.address}</td><td>{a.kind}</td><td>{a.state}</td><td>{a.generation}</td>
          <td>{a.kind === "alias" && (a.state === "reserved" ? <>
            <label>Reassign {a.address} to<select value={targets[a.address] ?? m.mailbox} onChange={e => setTargets({ ...targets, [a.address]: e.target.value })}>
              {mailboxes?.map(t => <option key={t.mailbox} value={t.mailbox}>{label(t.mailbox)}</option>)}
            </select></label>
            <button className="button secondary" aria-label={`Reassign ${a.address}`} disabled={!unlocked} onClick={() => {
              const to = targets[a.address] ?? m.mailbox;
              void act({ action: "reassign", address: a.address, mailbox: to },
                `Reassign ${a.address} to ${label(to)}? It delivers to that mailbox and may send from it while its owner is eligible, at a new generation. Mail delivered earlier stays where it is.`);
            }}>Reassign</button>
          </> : <button className="button secondary" aria-label={`Release ${a.address}`} disabled={!unlocked} onClick={() => void act({ action: "release", address: a.address },
            `Release ${a.address}? It becomes reserved: delivery to it and sending from it stop immediately. Mail already delivered stays in ${label(m.mailbox)}. It returns to service only when an administrator explicitly reassigns it.`)}>Release</button>)}</td>
        </tr>)}</tbody>
      </table></div>)}
      <h4>Add an alias</h4>
      <label>Mailbox<select value={addTo} onChange={e => setAddTo(e.target.value)}>
        <option value="">Choose a mailbox</option>
        {mailboxes?.map(m => <option key={m.mailbox} value={m.mailbox}>{label(m.mailbox)}</option>)}
      </select></label>
      <label>Alias address<input value={address} placeholder="sales@example.com" autoComplete="off" onChange={e => setAddress(e.target.value)} /></label>
      <p>Use a lowercase ASCII address on a configured mail domain. An address KyPost has ever held, or any KyIdentity primary address, is refused; a released alias returns only through Reassign.</p>
      <button className="button secondary" disabled={!unlocked || !addTo || !typed} onClick={() => void act({ action: "add", address: typed, mailbox: addTo })}>Add alias</button>
    </fieldset>
    {busy && <p role="status">Working…</p>}
  </div>;
}
