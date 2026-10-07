import { useEffect, useRef, useState } from "react";
import { useAuth } from "../../auth";
import { credentialFields, deriveCredential } from "../../api/auth";
import { deleteJSON, getJSON, HttpError, postJSON, toErrorMessage } from "../../api/client";
import { blockReasons, readSenderBlockAdded, readSenderBlockRemoved, readSenderBlocks, type BlockEvidence, type BlockKind, type BlockReason, type SenderBlock } from "../../api/nativeMail";
import { withSSOStepUp } from "../../api/stepup";
import { visible } from "../../lib/visibleText";

const time = (ms: number) => new Date(ms).toLocaleString(undefined, { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" });
const domainOf = (address: string) => address.slice(address.lastIndexOf("@") + 1);
const plural = (n: number, one: string, many: string) => `${n} ${n === 1 ? one : many}`;
const describe = (b: SenderBlock) => `${b.source} block${b.source === "automatic" ? ` (level ${b.level})` : ""} ${b.until === null ? "with no expiry" : `until ${time(b.until)}`}`;
// The datetime-local form of now, for the input's min.
const localNow = () => new Date(Date.now() - new Date().getTimezoneOffset() * 60_000).toISOString().slice(0, 16);

/** expiryMs reads a datetime-local value as local time; empty means no expiry. */
const asciiTrim = (v: string) => v.replace(/^[ \t]+|[ \t]+$/g, "");

export function expiryMs(input: string, now: number): number | null {
  if (!input) return null;
  const ms = new Date(input).getTime();
  if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(:\d{2}(\.\d+)?)?$/.test(input) || !Number.isSafeInteger(ms)) throw new Error("Expiry is not a valid date and time.");
  if (ms <= now) throw new Error("Expiry must be in the future.");
  return ms;
}

// What still refuses the sender once b is gone: blocks match an exact address or exact domain.
function afterRemoval(b: SenderBlock, list: SenderBlock[]): string {
  if (b.kind === "address") {
    const domain = domainOf(b.value);
    return list.some(x => x.kind === "domain" && x.value === domain)
      ? `Mail from this address stays refused by the domain block on ${visible(domain)}.`
      : "Its mail is accepted again from now on.";
  }
  const n = list.filter(x => x.kind === "address" && domainOf(x.value) === b.value).length;
  return n ? `Other mail from this domain is accepted again from now on; ${plural(n, "address block on this domain stays", "address blocks on this domain stay")} in force.` : "Its mail is accepted again from now on.";
}

export function SenderBlocks() {
  const auth = useAuth();
  if (!auth.authenticated || auth.role !== "admin") return null;
  return <SenderBlocksForm key={`${auth.userId}:${auth.username}:${auth.ssoSession}`} />;
}

// after: extra notice text from the re-read list.
type Change = { confirm: string; send: (body: object, headers: Record<string, string>) => Promise<unknown>; done: (answer: unknown) => { notice: string; warning: string }; after?: (fresh: SenderBlock[]) => string };

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
  async function read(): Promise<SenderBlock[] | null> {
    const generation = ++readGeneration.current;
    let raw: unknown;
    try {
      raw = await getJSON<unknown>("/api/admin/receiving/blocks");
    } catch (e: unknown) {
      if (!(e instanceof HttpError && e.status === 404)) throw e;
      if (live.current && generation === readGeneration.current) setOff(true);
      return null;
    }
    const list = readSenderBlocks(raw);
    if (!live.current || generation !== readGeneration.current) return null;
    setOff(false);
    setEvidence(list.evidence);
    setBlocks(list.blocks);
    return list.blocks;
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
      const fresh = await read();
      requireLive();
      setNotice(outcome.notice + (fresh && c.after ? c.after(fresh) : ""));
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
    if (!blocks) return;
    let until: number | null;
    setError("");
    try {
      until = expiryMs(expiry, Date.now());
    } catch (e: unknown) {
      setError(toErrorMessage(e, "Expiry is not a valid date and time."));
      return;
    }
    // Only ASCII padding is stripped: receivers compare non-ASCII local-part characters exactly.
    const want = { kind, value: asciiTrim(value), until, reason };
    // An unpaired surrogate cannot be sent as UTF-8, so it can never name a sender.
    if (/\p{Cs}/u.test(want.value)) {
      setError("The value contains an unpaired surrogate code unit, so it cannot be a sender.");
      return;
    }
    const shown = `${kind} ${visible(want.value)}`;
    const lower = want.value.replace(/[A-Z]/g, c => c.toLowerCase());
    const existing = blocks.find(b => b.kind === kind && b.value === lower);
    // A manual add evicts the soonest-expiring automatic blocks when the list is full; a replaced one keeps its ID.
    const automatic = blocks.filter(b => b.source === "automatic");
    void change({
      confirm: `Block the ${shown} ${until === null ? "with no expiry" : `until ${time(until)}`} (reason: ${reason})? Its mail is refused at reception from now on; mail already received is unaffected.${existing ? ` This replaces the existing ${describe(existing)}.` : ""}${kind === "domain" ? " Only this exact domain is blocked, not its subdomains." : ""} If the list is full, the automatic blocks that expire soonest are removed to make room. This deployment's own mail domains, and addresses on them, cannot be blocked.`,
      send: (body, headers) => postJSON<unknown>("/api/admin/receiving/blocks", { ...body, kind: want.kind, value: want.value, reason: want.reason, ...(until === null ? {} : { until }) }, headers),
      done: answer => {
        readSenderBlockAdded(answer, want);
        setValue(""); setExpiry("");
        return { notice: `Blocked the ${shown}.`, warning: "" };
      },
      after: fresh => {
        const now = Date.now();
        const gone = automatic.filter(b => (b.until === null || b.until > now) && !fresh.some(f => f.id === b.id)).length;
        return gone ? ` ${plural(gone, "automatic block was", "automatic blocks were")} removed to make room.` : "";
      },
    });
  }
  function remove(b: SenderBlock) {
    if (!blocks) return;
    const shown = `${b.source} ${b.kind} block on ${visible(b.value)}`;
    void change({
      confirm: `Remove the ${shown}? ${afterRemoval(b, blocks)} Automatic blocks of this exact ${b.kind} are then suppressed for 30 days.`,
      send: (body, headers) => deleteJSON<unknown>(`/api/admin/receiving/blocks/${encodeURIComponent(b.id)}`, body, headers),
      done: answer => ({ notice: `Removed the ${shown}.`, warning: readSenderBlockRemoved(answer, b.id) }),
    });
  }
  const unlocked = ssoSession || password.length > 0;
  const notices = <>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    <p className={notice ? "notice" : undefined} role="status">{notice}</p>
    <p className={warning ? "notice notice-warning" : undefined} role="status">{warning}</p>
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
    {evidence && !evidence.damaged && from === null && <p className="notice" role="note">Automatic blocks (address and domain) need Maddy with the Rspamd sidecar. Automatic domain blocks are not active yet: they start 30 days after the first authenticated mail is recorded.</p>}
    {from !== null && from > Date.now() && <p className="notice" role="note">Automatic domain blocks are not active until {time(from)}.</p>}
    {evidence?.automaticFull && <p className="notice notice-warning" role="note">Automatic blocking is full: new automatic blocks are refused until some expire. Add a manual domain block to cover a flood.</p>}
    {evidence?.goodFull && <p className="notice notice-warning" role="note">New domains are no longer protected from automatic domain blocks: the record of domains that sent authenticated mail is full.</p>}
    {automaticDomains > 0 && <p className="notice notice-warning" role="note">{automaticDomains === 1
      ? "1 automatic domain block refuses every sender at its domain, which can be a whole mail provider. Remove it if your users get mail from there."
      : `${automaticDomains} automatic domain blocks each refuse every sender at their domain, which can be a whole mail provider. Remove any your users get mail from.`}</p>}
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
      <label>Expires (optional; empty means no expiry)<input type="datetime-local" min={localNow()} value={expiry} onChange={e => setExpiry(e.target.value)} /></label>
      <label>Reason<select value={reason} onChange={e => setReason(blockReasons.find(r => r === e.target.value) ?? "other")}>
        {blockReasons.map(r => <option key={r} value={r}>{r}</option>)}
      </select></label>
      <p>A domain block matches that exact domain only, not its subdomains: block sub.example.com separately. Enter internationalized domains as A-labels (xn--…). This deployment's own mail domains cannot be blocked.</p>
      <button className="button" disabled={!unlocked || !asciiTrim(value)} onClick={add}>Block sender…</button>
    </fieldset>
    <fieldset className="config-card config-grid" disabled={busy || !blocks}>
      <legend>Blocks in force</legend>
      {blocks?.length === 0 && <p>No sender blocks.</p>}
      {!unlocked && !!blocks?.length && <p>Enter your account password above to add or remove blocks.</p>}
      {!!blocks?.length && <div className="users-table-wrap"><table className="users-table quarantine-table sender-blocks-table">
        <caption>Sender blocks in force</caption>
        <thead><tr><th scope="col">Kind</th><th scope="col">Value</th><th scope="col">Source</th><th scope="col">Level</th><th scope="col">Until</th><th scope="col">Created</th><th scope="col">Reason</th><th scope="col">Actions</th></tr></thead>
        <tbody>{blocks.map(b => <tr key={b.id}>
          <td>{b.kind}</td>
          <td className="sender-block-value">{visible(b.value)}</td>
          <td>{b.source === "automatic" && b.kind === "domain" ? <strong>automatic domain</strong> : b.source}</td>
          <td>{b.source === "automatic" ? b.level : "—"}</td>
          <td className="sender-block-time">{b.until === null ? "no expiry" : time(b.until)}</td>
          <td className="sender-block-time">{time(b.createdAt)}</td>
          <td>{b.reason}</td>
          <td className="quarantine-nowrap"><button className="button secondary" aria-label={`Remove ${b.kind} block ${visible(b.value)}`} disabled={!unlocked} onClick={() => remove(b)}>Remove…</button></td>
        </tr>)}</tbody>
      </table></div>}
    </fieldset>
    {busy && <p role="status">Working…</p>}
  </div>;
}
