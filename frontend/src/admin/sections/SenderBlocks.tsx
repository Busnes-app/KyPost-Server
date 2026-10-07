import { useEffect, useRef, useState } from "react";
import { useAuth } from "../../auth";
import { credentialFields, deriveCredential } from "../../api/auth";
import { deleteJSON, getJSON, HttpError, postJSON, toErrorMessage } from "../../api/client";
import { blockReasons, readSenderBlockAdded, readSenderBlockRemoved, readSenderBlocks, type BlockEvidence, type BlockKind, type BlockReason, type SenderBlock } from "../../api/nativeMail";
import { withSSOStepUp } from "../../api/stepup";
import { visible } from "../../lib/visibleText";

const time = (ms: number) => new Date(ms).toLocaleString(undefined, { timeZoneName: "short" });
const wholeDomain = "Automatic domain block: refuses every sender at this domain, which can be a whole mail provider";

/** expiryMs reads a datetime-local value as local time; empty means no expiry. */
export function expiryMs(input: string): number | null {
  if (!input) return null;
  const ms = new Date(input).getTime();
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?$/.test(input) || !Number.isSafeInteger(ms)) throw new Error("Expiry is not a valid date and time.");
  return ms;
}

export function SenderBlocks() {
  const auth = useAuth();
  if (!auth.authenticated || auth.role !== "admin") return null;
  return <SenderBlocksForm key={`${auth.userId}:${auth.username}:${auth.ssoSession}`} />;
}

type Change = { confirm: string; send: (body: object, headers: Record<string, string>) => Promise<unknown>; done: (answer: unknown) => { notice: string; warning: string } };

function SenderBlocksForm() {
  const ssoSession = useAuth().ssoSession === true;
  const live = useRef(false);
  const readGeneration = useRef(0);
  const inFlight = useRef(false);
  const [blocks, setBlocks] = useState<SenderBlock[] | null>(null);
  const [evidence, setEvidence] = useState<BlockEvidence | null>(null);
  const [off, setOff] = useState(false);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [warning, setWarning] = useState("");
  const [busy, setBusy] = useState(false);
  const [password, setPassword] = useState("");
  const [kind, setKind] = useState<BlockKind>("address");
  const [value, setValue] = useState("");
  const [expiry, setExpiry] = useState("");
  const [reason, setReason] = useState<BlockReason>("other");
  function requireLive() {
    if (!live.current) throw new Error("Sender blocks closed; reopen them to continue.");
  }
  async function read() {
    const generation = ++readGeneration.current;
    let raw: unknown;
    try {
      raw = await getJSON<unknown>("/api/admin/receiving/blocks");
    } catch (e: unknown) {
      if (!(e instanceof HttpError && e.status === 404)) throw e;
      if (live.current && generation === readGeneration.current) setOff(true);
      return;
    }
    const list = readSenderBlocks(raw);
    if (!live.current || generation !== readGeneration.current) return;
    setOff(false);
    setEvidence(list.evidence);
    setBlocks(list.blocks);
  }
  useEffect(() => {
    live.current = true;
    const generation = readGeneration.current + 1;
    void read().catch((e: unknown) => {
      if (live.current && generation === readGeneration.current) setError(toErrorMessage(e, "Unable to load sender blocks. Reload before changing them."));
    });
    return () => { live.current = false; readGeneration.current++; };
  }, []);
  // Same answered/unanswered rule as Quarantine: only an answered refusal returns the controls.
  async function change(c: Change) {
    if (!blocks || inFlight.current || !window.confirm(c.confirm)) return;
    inFlight.current = true;
    setBusy(true); setError(""); setNotice(""); setWarning("");
    const accountPassword = password;
    setPassword("");
    let answered = false, committed = false, unanswered = false;
    try {
      const body = ssoSession ? {} : credentialFields(await deriveCredential("", accountPassword));
      requireLive();
      const result = await withSSOStepUp(async (headers) => {
        requireLive();
        unanswered = true;
        const answer = await c.send(body, headers).catch((e: unknown) => { if (e instanceof HttpError) unanswered = false; throw e; });
        unanswered = false;
        return answer;
      });
      answered = true;
      requireLive();
      const outcome = c.done(result);
      committed = true;
      setBlocks(null);
      await read();
      requireLive();
      setNotice(outcome.notice);
      setWarning(outcome.warning);
    } catch (e: unknown) {
      if (live.current) {
        const message = toErrorMessage(e, "Sender block change failed. Reload before retrying an uncertain change.");
        if (!answered && !unanswered && !(e instanceof HttpError)) {
          setError(`Not confirmed; nothing changed. ${message}`);
          return;
        }
        setBlocks(null);
        setError(committed ? `Change saved; reload to see current sender blocks. ${message}` : message);
        if (!committed && e instanceof HttpError) await read().catch(() => undefined);
      }
    } finally {
      inFlight.current = false;
      if (live.current) { setPassword(""); setBusy(false); }
    }
  }
  function add() {
    let until: number | null;
    try {
      until = expiryMs(expiry);
    } catch (e: unknown) {
      setError(toErrorMessage(e, "Expiry is not a valid date and time."));
      return;
    }
    const want = { kind, value: value.trim(), until, reason };
    const shown = `${kind} ${visible(want.value)}`;
    void change({
      confirm: `Block the ${shown} ${until === null ? "with no expiry" : `until ${time(until)}`} (reason: ${reason})? Its mail is refused at reception from now on; mail already received is unaffected.${kind === "domain" ? " Only this exact domain is blocked, not its subdomains." : ""} This deployment's own mail domains, and addresses on them, cannot be blocked.`,
      send: (body, headers) => postJSON<unknown>("/api/admin/receiving/blocks", { ...body, kind: want.kind, value: want.value, reason: want.reason, ...(until === null ? {} : { until }) }, headers),
      done: answer => {
        readSenderBlockAdded(answer, want);
        setValue(""); setExpiry("");
        return { notice: `Blocked the ${shown}.`, warning: "" };
      },
    });
  }
  function remove(b: SenderBlock) {
    const shown = `${b.source} ${b.kind} block on ${visible(b.value)}`;
    void change({
      confirm: `Remove the ${shown}? Its mail is accepted again from now on. Automatic blocks of this exact ${b.kind} are then suppressed for 30 days.`,
      send: (body, headers) => deleteJSON<unknown>(`/api/admin/receiving/blocks/${encodeURIComponent(b.id)}`, body, headers),
      done: answer => ({ notice: `Removed the ${shown}.`, warning: readSenderBlockRemoved(answer, b.id) }),
    });
  }
  const unlocked = ssoSession || password.length > 0;
  const notices = <>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {notice && <p className="notice" role="status">{notice}</p>}
    {warning && <p className="notice notice-warning" role="status">{warning}</p>}
  </>;
  if (off) return <div className="config-section">
    <h3>Sender blocks</h3>
    {notices}
    <p>Native mail is off on this server, so there are no sender blocks to manage.</p>
  </div>;
  const from = evidence?.domainBlocksFrom ?? null;
  const automaticDomains = blocks?.filter(b => b.source === "automatic" && b.kind === "domain").length ?? 0;
  return <div className="config-section">
    <h3>Sender blocks</h3>
    <p>Envelope senders refused at reception, by both receiving profiles. Blocks never delete mail already received. Bounces (the empty sender) are never blocked.</p>
    {evidence?.damaged && <p className="notice notice-warning" role="note">Automatic-block evidence is unreadable. A malformed file is set aside at the next write and counting restarts; any other read failure needs storage repair. Blocks in force are unaffected.</p>}
    {evidence?.resetAt != null && <p className="notice notice-warning" role="note">Automatic-block evidence was damaged and restarted at {time(evidence.resetAt)}; counts before then are lost.</p>}
    {evidence && from === null && <p className="notice" role="note">Automatic domain blocks are not active yet: they start 30 days after the first authenticated mail is recorded (Maddy with Rspamd only).</p>}
    {from !== null && from > Date.now() && <p className="notice" role="note">Automatic domain blocks are not active until {time(from)}.</p>}
    {evidence?.automaticFull && <p className="notice notice-warning" role="note">Automatic blocking is full: new automatic blocks are refused until some expire. Add a manual domain block to cover a flood.</p>}
    {evidence?.goodFull && <p className="notice notice-warning" role="note">New domains are no longer protected from automatic domain blocks: the record of domains that sent authenticated mail is full.</p>}
    {automaticDomains > 0 && <p className="notice notice-warning" role="note">{automaticDomains === 1 ? "1 automatic domain block refuses" : `${automaticDomains} automatic domain blocks refuse`} every sender at that domain, which can be a whole mail provider. Remove it if your users get mail from there.</p>}
    {notices}
    {!blocks && <p>{error ? "Sender blocks unavailable. Reload this page before making changes." : "Loading sender blocks…"}</p>}
    <fieldset className="config-card config-grid" disabled={busy || !blocks}>
      <legend>Confirm each action</legend>
      {ssoSession ? <p>Confirm each action with KySignOn.</p> : <label>Account password<input type="password" autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} /></label>}
    </fieldset>
    <fieldset className="config-card config-grid" disabled={busy || !blocks}>
      <legend>Add a block</legend>
      <label>Kind<select value={kind} onChange={e => setKind(e.target.value === "domain" ? "domain" : "address")}>
        <option value="address">address</option><option value="domain">domain</option>
      </select></label>
      <label>Sender {kind}<input value={value} onChange={e => setValue(e.target.value)} autoComplete="off" spellCheck={false} /></label>
      <label>Expires (optional; empty means no expiry)<input type="datetime-local" value={expiry} onChange={e => setExpiry(e.target.value)} /></label>
      <label>Reason<select value={reason} onChange={e => setReason(blockReasons.find(r => r === e.target.value) ?? "other")}>
        {blockReasons.map(r => <option key={r} value={r}>{r}</option>)}
      </select></label>
      <p>A domain block matches that exact domain only, not its subdomains: block sub.example.com separately. Enter internationalized domains as A-labels (xn--…). This deployment's own mail domains cannot be blocked.</p>
      <button className="button" disabled={!unlocked || !value.trim()} onClick={add}>Block sender…</button>
    </fieldset>
    <fieldset className="config-card config-grid" disabled={busy || !blocks}>
      <legend>Blocks in force</legend>
      {blocks?.length === 0 && <p>No sender blocks.</p>}
      {!!blocks?.length && <div className="users-table-wrap"><table className="users-table quarantine-table">
        <caption>Sender blocks in force</caption>
        <thead><tr><th scope="col">Kind</th><th scope="col">Value</th><th scope="col">Source</th><th scope="col">Level</th><th scope="col">Until</th><th scope="col">Created</th><th scope="col">Reason</th><th scope="col">Actions</th></tr></thead>
        <tbody>{blocks.map(b => <tr key={b.id}>
          <td>{b.kind}</td>
          <td className="quarantine-sender">{visible(b.value)}</td>
          <td>{b.source === "automatic" && b.kind === "domain" ? <strong>{wholeDomain}</strong> : b.source}</td>
          <td>{b.source === "automatic" ? b.level : "—"}</td>
          <td>{b.until === null ? "no expiry" : time(b.until)}</td>
          <td>{time(b.createdAt)}</td>
          <td>{b.reason}</td>
          <td className="quarantine-nowrap"><button className="button secondary" aria-label={`Remove ${b.kind} block ${visible(b.value)}`} disabled={!unlocked} onClick={() => remove(b)}>Remove…</button></td>
        </tr>)}</tbody>
      </table></div>}
    </fieldset>
    {busy && <p role="status">Working…</p>}
  </div>;
}
