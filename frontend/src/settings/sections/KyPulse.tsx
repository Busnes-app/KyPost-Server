import { useEffect, useState } from "react";
import { deleteJSON, getJSON, postJSON, putJSON, toErrorMessage } from "../../api/client";

type State = {
  available: boolean;
  enabled: boolean;
  own: string[];
  recipients: { address: string; confirmed: boolean }[];
};

const path = "/api/settings/kypulse";

/** The user's opt-in for KyPulse alert mail and the addresses it may go to. */
export function KyPulse() {
  const [state, setState] = useState<State | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [address, setAddress] = useState("");

  useEffect(() => {
    let live = true;
    getJSON<State>(path)
      .then(s => { if (live) setState(s); })
      .catch((e: unknown) => { if (live) setError(toErrorMessage(e, "Could not read your KyPulse setting.")); });
    return () => { live = false; };
  }, []);

  const run = (request: () => Promise<State>, done: string, fallback: string) => {
    setBusy(true);
    setError("");
    setNotice("");
    request()
      .then(s => { setState(s); setNotice(done); })
      .catch((e: unknown) => setError(toErrorMessage(e, fallback)))
      .finally(() => setBusy(false));
  };

  const add = () => {
    const value = address.trim();
    if (!value) return;
    run(() => postJSON<State>(`${path}/recipients`, { address: value }).then(s => { setAddress(""); return s; }),
      `A confirmation link was sent to ${value}.`, "Could not add that address.");
  };

  return <div className="config-section">
    <h3>KyPulse</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {!state && !error && <p>Loading…</p>}
    {state && !state.available && <p>Alert mail with KyPulse is not connected on this server. It needs KyPulse and KyPost to sign in through KyIdentity, which pairs them. It lets KyPulse send monitoring alerts from your mailbox to addresses you list.</p>}
    {state?.available && <>
      <label>
        <input type="checkbox" checked={state.enabled} disabled={busy}
          onChange={e => run(() => putJSON<State>(path, { enabled: e.target.checked }), "", "Could not save your KyPulse setting.")} />
        {" "}Let KyPulse send alerts from my mailbox
      </label>
      <p>KyPulse can then send plain-text alerts as you, through your own outbox, to the addresses below. They appear in Sent and count toward your sending quota.</p>
    </>}
    {state && <>
      <h4>Your own addresses</h4>
      <p className="config-muted">Always allowed, no confirmation needed.</p>
      {state.own.length > 0 ? <ul>{state.own.map(a => <li key={a}>{a}</li>)}</ul> : <p className="config-muted">None yet: set up your outgoing mail first.</p>}
      <h4>Other addresses</h4>
      <p className="config-muted">Up to 10. Each gets a link from your mailbox and receives no alert until someone opens it and presses Confirm.</p>
      {state.recipients.length > 0 ? <div className="config-status-card">
        {state.recipients.map(r => <div key={r.address} style={{ display: "flex", flexWrap: "wrap", alignItems: "center", justifyContent: "space-between", gap: 8, padding: "6px 0" }}>
          <span style={{ overflowWrap: "anywhere" }}>{r.address}</span>
          <span style={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: 8 }}>
            <span className="ky-status" data-ky-status={r.confirmed ? "success" : "warning"}>{r.confirmed ? "Confirmed" : "Pending"}</span>
            {!r.confirmed && <button type="button" disabled={busy}
              onClick={() => run(() => postJSON<State>(`${path}/recipients/resend`, { address: r.address }), `A new link was sent to ${r.address}; the old one no longer works.`, "Could not resend the link.")}>
              Resend
            </button>}
            <button type="button" disabled={busy} aria-label={`Remove ${r.address}`}
              onClick={() => run(() => deleteJSON<State>(`${path}/recipients`, { address: r.address }), "", "Could not remove that address.")}>
              Remove
            </button>
          </span>
        </div>)}
      </div> : <p className="config-muted">No other addresses.</p>}
      {state.recipients.length < 10 && <div className="config-actions">
        <label>
          <div>Add an address</div>
          <input type="email" value={address} onChange={e => setAddress(e.target.value)} placeholder="ops@example.com" />
        </label>
        <button type="button" onClick={add} disabled={busy || !address.trim()}>Send confirmation link</button>
      </div>}
      {notice && <p className="config-muted" role="status">{notice}</p>}
    </>}
  </div>;
}
