import { useEffect, useRef, useState } from "react";
import { useAuth } from "../../auth";
import { credentialFields, deriveCredential } from "../../api/auth";
import { getJSON, HttpError, postJSON, toErrorMessage } from "../../api/client";
import { readQuarantine, readQuarantineChange, type QuarantinedDelivery } from "../../api/nativeMail";
import { listUsers } from "../../api/users";
import { withSSOStepUp } from "../../api/stepup";

const PAGE = 100;
// Envelope text is sender-controlled: control, format (bidi, zero-width) and
// line/paragraph separators are shown as code points, never applied.
function visible(value: string): string {
  return value.replace(/[\p{Cc}\p{Cf}\u2028\u2029]/gu, c => `[U+${c.codePointAt(0)!.toString(16).toUpperCase().padStart(4, "0")}]`);
}
// "." and ".." are URL dot segments: fetch would resolve them to another route.
const addressable = (d: QuarantinedDelivery) => ![d.gateway, d.id].some(v => v === "." || v === "..");
function size(bytes: number) {
  return bytes < 1024 ? `${bytes} B` : bytes < 1024 * 1024 ? `${(bytes / 1024).toFixed(1)} KiB` : `${(bytes / 1024 / 1024).toFixed(1)} MiB`;
}

export function Quarantine() {
  const auth = useAuth();
  if (!auth.authenticated || auth.role !== "admin") return null;
  return <QuarantineForm key={`${auth.userId}:${auth.username}:${auth.ssoSession}`} />;
}

function QuarantineForm() {
  const ssoSession = useAuth().ssoSession === true;
  const live = useRef(false);
  const readGeneration = useRef(0);
  const inFlight = useRef(false);
  const [deliveries, setDeliveries] = useState<QuarantinedDelivery[] | null>(null);
  const [more, setMore] = useState(false);
  const [off, setOff] = useState(false);
  const [names, setNames] = useState(new Map<string, string>());
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [warning, setWarning] = useState("");
  const [busy, setBusy] = useState(false);
  const [password, setPassword] = useState("");
  function requireLive() {
    if (!live.current) throw new Error("Quarantine closed; reopen it to continue.");
  }
  // Reads the first page again, or appends the page after the shown list.
  async function read(append: QuarantinedDelivery[] = []) {
    const generation = ++readGeneration.current;
    const after = append[append.length - 1]?.sequence ?? 0;
    let raw: unknown;
    try {
      raw = await getJSON<unknown>(`/api/admin/receiving/quarantine${after ? `?after=${after}` : ""}`);
    } catch (e: unknown) {
      if (!(e instanceof HttpError && e.status === 404)) throw e;
      if (live.current && generation === readGeneration.current) setOff(true);
      return;
    }
    const page = readQuarantine(raw, after);
    if (!live.current || generation !== readGeneration.current) return;
    setOff(false);
    setMore(page.length === PAGE);
    setDeliveries([...append, ...page]);
  }
  useEffect(() => {
    live.current = true;
    // Names are display only: without them the table shows user IDs.
    listUsers().then(users => {
      if (live.current) setNames(new Map(users.filter(u => typeof u.id === "string" && typeof u.username === "string").map(u => [u.id, u.username])));
    }).catch(() => undefined);
    const generation = readGeneration.current + 1;
    void read().catch((e: unknown) => {
      if (live.current && generation === readGeneration.current) setError(toErrorMessage(e, "Unable to load quarantine. Reload before changing it."));
    });
    return () => { live.current = false; readGeneration.current++; };
  }, []);
  async function loadMore() {
    if (!deliveries || inFlight.current) return;
    inFlight.current = true;
    setBusy(true); setError("");
    try {
      await read(deliveries);
    } catch (e: unknown) {
      if (live.current) setError(toErrorMessage(e, "Unable to load more quarantined mail."));
    } finally {
      inFlight.current = false;
      if (live.current) setBusy(false);
    }
  }
  function recipient(r: QuarantinedDelivery["recipients"][number]) {
    return `${visible(r.address)} → mailbox ${visible(r.mailbox)} / ${r.user ? visible(names.get(r.user) ?? r.user) : "mailbox gone"}`;
  }
  async function act(d: QuarantinedDelivery, action: "release" | "discard") {
    if (!deliveries || inFlight.current) return;
    const what = `delivery ${visible(d.id)} from ${d.sender ? visible(d.sender) : "an empty sender"}`;
    const listed = d.recipients.slice(0, 10).map(recipient).join("; ") + (d.recipients.length > 10 ? `; and ${d.recipients.length - 10} more listed in the table` : "");
    if (!window.confirm(action === "release"
      ? `Release ${what}? It goes only to the mailboxes it was frozen to when it arrived: ${listed || "none"}. It never goes to an address's current owner. Each mailbox must still be active and its owner admitted.`
      : `Discard ${what}? Its bytes are deleted permanently: it can never be released afterwards, and a re-pickup or receiver replay will not bring it back. If a release had started, some of its mailboxes may already hold it; those copies stay and the result is recorded as partially released.`)) return;
    inFlight.current = true;
    setBusy(true); setError(""); setNotice(""); setWarning("");
    const accountPassword = password;
    setPassword("");
    let committed = false;
    try {
      const body = ssoSession ? {} : credentialFields(await deriveCredential("", accountPassword));
      requireLive();
      const result = await withSSOStepUp((headers) => {
        requireLive();
        return postJSON<unknown>(`/api/admin/receiving/quarantine/${encodeURIComponent(d.gateway)}/${encodeURIComponent(d.id)}/${action}`, body, headers);
      });
      requireLive();
      const outcome = readQuarantineChange(result, d.gateway, d.id, action);
      committed = true;
      // A committed change followed by an unreadable list must not leave stale controls enabled.
      setDeliveries(null);
      await read();
      requireLive();
      if (outcome === "partially_released") {
        setWarning(`Discarded ${what}, recorded as partially released: an earlier release reached some of its mailboxes, and those copies stay.`);
      } else {
        setNotice(outcome === "released" ? `Released ${what} to its original mailboxes.` : `Discarded ${what}. Its bytes are deleted.`);
      }
    } catch (e: unknown) {
      if (live.current) {
        setDeliveries(null);
        const message = toErrorMessage(e, "Quarantine change failed. Reload before retrying an uncertain change.");
        setError(committed ? `Change saved; reload to see current quarantine. ${message}` : message);
        // The server answered, so controls return only if a fresh list validates.
        // A request that never got an answer stays locked until reload.
        if (!committed && e instanceof HttpError) await read().catch(() => undefined);
      }
    } finally {
      inFlight.current = false;
      if (live.current) { setPassword(""); setBusy(false); }
    }
  }
  const unlocked = ssoSession || password.length > 0;
  if (off) return <div className="config-section">
    <h3>Quarantine</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    <p>Native mail is off on this server, so there is no quarantine to review.</p>
  </div>;
  return <div className="config-section">
    <h3>Quarantine</h3>
    <p>Received mail held back from delivery, usually because an address changed mailbox after it arrived. Only the envelope is shown, never the body or subject. The sender is not verified.</p>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {notice && <p className="notice" role="status">{notice}</p>}
    {warning && <p className="notice notice-warning" role="status">{warning}</p>}
    {!deliveries && <p>{error ? "Quarantine unavailable. Reload this page before making changes." : "Loading quarantine…"}</p>}
    <fieldset className="config-card config-grid" disabled={busy || !deliveries}>
      <legend>Confirm each action</legend>
      {ssoSession ? <p>Confirm each action with KySignOn.</p> : <label>Account password<input type="password" autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} /></label>}
    </fieldset>
    <fieldset className="config-card config-grid" disabled={busy || !deliveries}>
      <legend>Quarantined mail</legend>
      {deliveries?.length === 0 && <p>No quarantined mail.</p>}
      {!!deliveries?.length && <div className="users-table-wrap"><table className="users-table">
        <caption>Quarantined deliveries, oldest first</caption>
        <thead><tr><th scope="col">Received</th><th scope="col">Sender</th><th scope="col">Recipients</th><th scope="col">Size</th><th scope="col">Gateway / ID</th><th scope="col">Actions</th></tr></thead>
        <tbody>{deliveries.map(d => <tr key={d.sequence}>
          <td>{new Date(d.receivedAt).toLocaleString()}</td>
          <td>{d.sender ? visible(d.sender) : "(empty sender)"}</td>
          <td>{d.recipients.length ? <ul>{d.recipients.map((r, i) => <li key={i}>{recipient(r)}</li>)}</ul> : "none"}</td>
          <td>{size(d.size)}</td>
          <td>{`${visible(d.gateway)} / ${visible(d.id)}`}</td>
          <td>{addressable(d) ? <>
            <button className="button secondary" aria-label={`Release ${visible(d.id)}`} disabled={!unlocked} onClick={() => void act(d, "release")}>Release</button>
            <button className="button secondary" aria-label={`Discard ${visible(d.id)}`} disabled={!unlocked} onClick={() => void act(d, "discard")}>Discard</button>
          </> : "Use the CLI: this ID cannot be sent in a URL."}</td>
        </tr>)}</tbody>
      </table></div>}
      {more && <button className="button secondary" onClick={() => void loadMore()}>Load more</button>}
    </fieldset>
    {busy && <p role="status">Working…</p>}
  </div>;
}
