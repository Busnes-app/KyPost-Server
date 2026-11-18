import { useEffect, useState } from "react";
import { HttpError, deleteJSON, getJSON, postJSON, putJSON, toErrorMessage } from "../../api/client";

export type MaskedAddress = { address: string; note: string; state: "active" | "paused" | "deleted"; createdAt: number; expiresAt?: number };
type State = { plusAddressing: boolean; maskedDomains: string[]; masked: MaskedAddress[]; maskedLimit: number; maskedPerDay: number };

const EXPIRY = [
  { value: "", label: "Masked: lasts until you delete it" },
  { value: "24h", label: "Disposable: 24 hours" },
  { value: "7d", label: "Disposable: 7 days" },
  { value: "30d", label: "Disposable: 30 days" },
];

const when = (unix: number) => new Date(unix * 1000).toLocaleString();

/** Plus addressing and the user's own masked and disposable addresses (hosted mail only). */
export function Addresses() {
  const [state, setState] = useState<State | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [domain, setDomain] = useState("");
  const [note, setNote] = useState("");
  const [expires, setExpires] = useState("");

  const load = () => getJSON<State>("/api/addresses").then(setState);

  useEffect(() => {
    let live = true;
    getJSON<State>("/api/addresses")
      .then((s) => { if (live) setState(s); })
      .catch((e: unknown) => {
        if (!live) return;
        if (e instanceof HttpError && e.status === 404) setUnavailable(true);
        else setError(toErrorMessage(e, "Could not read your address settings."));
      });
    return () => { live = false; };
  }, []);

  // request may answer its own notice (a string); otherwise done is shown.
  const run = (request: () => Promise<unknown>, done: string, fallback: string) => {
    setBusy(true);
    setError("");
    setNotice("");
    request()
      .then(async (said) => {
        await load();
        setNotice(typeof said === "string" ? said : done);
      })
      .catch((e: unknown) => setError(toErrorMessage(e, fallback)))
      .finally(() => setBusy(false));
  };

  const create = () => {
    const chosen = domain || state?.maskedDomains[0] || "";
    run(
      () => postJSON<MaskedAddress>("/api/addresses/masked", { domain: chosen, note: note.trim(), expires }).then((m) => {
        setNote("");
        return `Created ${m.address}.`;
      }),
      "",
      "Could not create a masked address."
    );
  };

  const setMasked = (m: MaskedAddress, body: { state: "active" | "paused" }) =>
    run(() => putJSON(`/api/addresses/masked/${encodeURIComponent(m.address)}`, body),
      body.state === "paused" ? `${m.address} is paused: mail to it is refused.` : `${m.address} receives mail again.`,
      "Could not change that address.");

  const remove = (m: MaskedAddress) => {
    if (!window.confirm(`Delete ${m.address}? Mail to it is refused from now on, and the address is never given to anyone again. This cannot be undone.`)) return;
    run(() => deleteJSON(`/api/addresses/masked/${encodeURIComponent(m.address)}`), `${m.address} is deleted.`, "Could not delete that address.");
  };

  if (unavailable) {
    return <div className="config-section">
      <h3>Addresses</h3>
      <p>Plus addressing and masked addresses are available for mailboxes hosted on this server.</p>
    </div>;
  }

  const current = state?.masked.filter((m) => m.state !== "deleted") ?? [];
  const deleted = state?.masked.filter((m) => m.state === "deleted") ?? [];
  return <div className="config-section">
    <h3>Addresses</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {!state && !error && <p>Loading…</p>}
    {state && <>
      <h4>Plus addressing</h4>
      <label>
        <input type="checkbox" checked={state.plusAddressing} disabled={busy}
          onChange={(e) => run(() => putJSON("/api/addresses/plus", { enabled: e.target.checked }), "", "Could not save your plus addressing setting.")} />
        {" "}Accept mail sent to plus addresses
      </label>
      <p className="config-muted">Give out <code>you+shop@…</code> instead of your address: it arrives in your inbox, you can sort it with a mailbox rule on the tag after <code>+</code>, and replies go out from the same plus address. Turn this off to refuse such mail as if the address did not exist.</p>

      <h4>Masked addresses</h4>
      <p className="config-muted">A random address that forwards to your inbox, so a shop or newsletter never learns your real one. Replies and new mail you send from it go out as the masked address. Pause one to refuse its mail; delete it when it gets spam. A deleted address is never given to anyone else.</p>
      {state.maskedDomains.length === 0 ? <p>Your administrator has not set up masked addresses on this server yet.</p> : <>
        <div className="config-actions">
          {state.maskedDomains.length > 1 && <label>
            <div>Domain</div>
            <select value={domain || state.maskedDomains[0]} onChange={(e) => setDomain(e.target.value)}>
              {state.maskedDomains.map((d) => <option key={d} value={d}>{d}</option>)}
            </select>
          </label>}
          <label>
            <div>Note (optional)</div>
            <input type="text" maxLength={100} value={note} onChange={(e) => setNote(e.target.value)} placeholder="used for: example-shop" />
          </label>
          <label>
            <div>Kind</div>
            <select value={expires} onChange={(e) => setExpires(e.target.value)}>
              {EXPIRY.map((x) => <option key={x.value} value={x.value}>{x.label}</option>)}
            </select>
          </label>
          <button type="button" onClick={create} disabled={busy || current.length >= state.maskedLimit}>Create address</button>
        </div>
        <p className="config-muted">{current.length} of {state.maskedLimit} in use; up to {state.maskedPerDay} new ones a day.</p>
      </>}
      {current.length > 0 && <div className="config-status-card">
        {current.map((m) => <div key={m.address} style={{ display: "flex", flexWrap: "wrap", alignItems: "center", justifyContent: "space-between", gap: 8, padding: "6px 0" }}>
          <span style={{ overflowWrap: "anywhere" }}>
            <strong>{m.address}</strong>
            {m.note && <> · {m.note}</>}
            {m.expiresAt ? <span className="config-muted"> · expires {when(m.expiresAt)}</span> : null}
          </span>
          <span style={{ display: "flex", flexWrap: "wrap", alignItems: "center", gap: 8 }}>
            <span className="ky-status" data-ky-status={m.state === "active" ? "success" : "warning"}>{m.state === "active" ? "Active" : "Paused"}</span>
            <button type="button" disabled={busy} aria-label={`${m.state === "active" ? "Pause" : "Resume"} ${m.address}`}
              onClick={() => setMasked(m, { state: m.state === "active" ? "paused" : "active" })}>
              {m.state === "active" ? "Pause" : "Resume"}
            </button>
            <button type="button" disabled={busy} aria-label={`Delete ${m.address}`} onClick={() => remove(m)}>Delete</button>
          </span>
        </div>)}
      </div>}
      {deleted.length > 0 && <details>
        <summary>Deleted addresses ({deleted.length})</summary>
        <ul>{deleted.map((m) => <li key={m.address}>{m.address}{m.note && ` · ${m.note}`}</li>)}</ul>
      </details>}
      {notice && <p className="config-muted" role="status">{notice}</p>}
    </>}
  </div>;
}
