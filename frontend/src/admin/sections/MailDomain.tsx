import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import { useAuth } from "../../auth";
import { credentialFields, deriveCredential } from "../../api/auth";
import { deleteJSON, HttpError, postJSON, putJSON, toErrorMessage } from "../../api/client";
import { loadNativeMail, readMailRelayCheck, type MailDomainEntry, type NativeMailSetup } from "../../api/nativeMail";
import { withSSOStepUp } from "../../api/stepup";

type Action = "claim" | "rotate" | "verify" | "retire" | "relay" | "domains" | "check";

function retireWarning(domain: string) {
  return `Retire ${domain}? KyPost refuses while any address on it is active, queued or retryable mail sends from it, accepted incoming mail is waiting for it, or the relay still sends for it, or while a restore hold is in place. Existing address records are kept. You can re-add it later, but it then needs a new TXT challenge and fresh DNS proof before it carries mail.`;
}

function domainStatus(d: MailDomainEntry, now: number) {
  if (d.retired) return { label: "Retired", detail: "No mail is routed, sent or allocated on this domain. Its address records are kept; re-adding it starts a new TXT challenge." };
  if (d.established) return { label: "Established", detail: `Ownership established. Keep this TXT record; KyPost rechecks DNS before native operations.${d.verifiedUntil > now ? ` Last DNS match holds until ${new Date(d.verifiedUntil * 1000).toLocaleString()}.` : ""}` };
  if (d.expiresAt > now) return { label: "Awaiting verification", detail: `Publish this TXT record and verify it before ${new Date(d.expiresAt * 1000).toLocaleString()}.` };
  return { label: "Lapsed", detail: "The challenge expired before verification. Replace the challenge, publish the new record and verify it." };
}

export function MailDomain() {
  const auth = useAuth();
  if (!auth.authenticated || auth.role !== "admin") return null;
  return <MailDomainForm key={`${auth.userId}:${auth.username}:${auth.ssoSession}`} />;
}

function MailDomainForm() {
  const ssoSession = useAuth().ssoSession === true;
  const live = useRef(false);
  const refreshGeneration = useRef(0);
  const inFlight = useRef(false);
  const [setup, setSetup] = useState<NativeMailSetup | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [domain, setDomain] = useState("");
  const [host, setHost] = useState("");
  const [port, setPort] = useState("465");
  const [username, setUsername] = useState("");
  const [secret, setSecret] = useState("");
  const [password, setPassword] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  function requireLive() {
    if (!live.current) throw new Error("Mail setup closed; reopen it to continue.");
  }
  async function refresh() {
    const generation = ++refreshGeneration.current;
    const next = await loadNativeMail();
    if (!live.current || generation !== refreshGeneration.current) return;
    setSetup(next);
    if (next.relay.kind === "configured") {
      setHost(next.relay.host);
      setPort(String(next.relay.port));
    }
    if (next.mode === "set") {
      const founding = next.domains.domains.find(d => d.domain === next.domains.founding && d.established);
      setSelected(next.relay.kind === "configured" ? next.relay.domains : founding ? [founding.domain] : []);
    }
    return next;
  }
  useEffect(() => {
    live.current = true;
    const generation = refreshGeneration.current + 1;
    void refresh().catch((e: unknown) => {
      if (live.current && generation === refreshGeneration.current) setError(toErrorMessage(e, "Unable to load mail setup. Reload before changing configuration."));
    });
    return () => { live.current = false; refreshGeneration.current++; };
  }, []);
  // target names the domain for domain-set actions; the single-domain view leaves it empty.
  async function act(action: Action, target = "") {
    if (!setup || inFlight.current || action === "check" && setup.relay.kind !== "configured") return;
    if (action === "rotate" && !window.confirm(`Replace the TXT challenge${target ? ` for ${target}` : ""}? This clears domain verification and pauses native account allocation and sending${target ? " on it" : ""} until the new record is verified.`)) return;
    if (action === "retire" && !window.confirm(retireWarning(target))) return;
    if (action === "relay" && setup.relay.kind === "configured" && !window.confirm("Replace the relay profile? This rotates its generation and stops unclaimed deliveries queued under the old profile. Check outbox and provider evidence before resubmitting to avoid duplicates.")) return;
    inFlight.current = true;
    setBusy(true); setError(""); setNotice("");
    // Freeze the request before credential derivation or KySignOn confirmation.
    const set = setup.mode === "set";
    const fields = action === "relay"
      ? { host, port: Number(port), smtpUsername: username, smtpPassword: secret, ...(set ? { domains: selected } : {}) }
      : action === "domains" ? { domains: selected }
      : action === "check" && setup.relay.kind === "configured" ? { expectedGeneration: setup.relay.generation }
      : action === "verify" || action === "retire" ? {}
      : { domain: target || (setup.mode === "single" && setup.domain.kind === "configured" ? setup.domain.domain : domain) };
    const path = `/api/admin/mail-domains/${encodeURIComponent(target)}`;
    const accountPassword = password;
    setPassword(""); setSecret(""); setUsername("");
    try {
      const credential = ssoSession ? {} : credentialFields(await deriveCredential("", accountPassword));
      requireLive();
      const body = { ...fields, ...credential };
      const result = await withSSOStepUp((headers) => {
        requireLive();
        return action === "check" ? postJSON<unknown>("/api/admin/mail-relay/test", body, headers)
          : action === "relay" || action === "domains" ? putJSON<unknown>("/api/admin/mail-relay", body, headers)
          : action === "retire" ? deleteJSON<unknown>(path, body, headers)
          : action === "verify" ? postJSON<unknown>(set ? `${path}/verify` : "/api/admin/mail-domain/verify", body, headers)
          : set ? postJSON<unknown>("/api/admin/mail-domains", body, headers)
          : putJSON<unknown>("/api/admin/mail-domain", body, headers);
      });
      requireLive();
      if (action === "check") readMailRelayCheck(result, setup.relay);
      // A successful mutation followed by an unreadable GET must not leave stale controls enabled.
      setSetup(null);
      const refreshed = await refresh();
      requireLive();
      if (action === "check" && refreshed) readMailRelayCheck(result, refreshed.relay);
      if (action === "claim") setDomain("");
      setNotice(action === "check" ? "TLS and SMTP authentication passed for the saved relay. No email was sent; delivery remains untested."
        : action === "relay" ? "Relay saved. No provider connection or test email was made."
        : action === "domains" ? "Relay sending domains saved. Credentials and relay generation are unchanged."
        : action === "retire" ? `${target} retired. Its address records are kept; re-adding it needs a new TXT challenge and DNS proof.`
        : action === "verify" ? "TXT record matched. KyPost rechecks DNS before native operations."
        : "Challenge created. Publish the exact TXT record below, then verify it.");
    } catch (e: unknown) {
      if (live.current) {
        setSetup(null);
        setError(toErrorMessage(e, "Mail setup failed. Reload status before retrying an uncertain change."));
        // The server answered, so controls return only if a fresh status read validates.
        // A request that never got an answer stays locked until reload.
        if (e instanceof HttpError) await refresh().catch(() => undefined);
      }
    } finally {
      inFlight.current = false;
      if (live.current) { setPassword(""); setSecret(""); setUsername(""); setBusy(false); }
    }
  }
  const unlocked = ssoSession || password.length > 0;
  const claim = setup?.mode === "single" ? setup.domain : undefined;
  const domainSet = setup?.mode === "set" ? setup.domains : undefined;
  const inService = domainSet?.domains.filter(d => !d.retired) ?? [];
  const relay = setup?.relay;
  const relayDomains = relay?.kind === "configured" ? relay.domains : [];
  const domainsChanged = [...selected].sort().join() !== [...relayDomains].sort().join();
  const canSave = domainSet ? selected.length > 0 : claim?.kind === "configured" && claim.established;
  const canCheck = domainSet ? relayDomains.some(d => inService.some(l => l.domain === d && l.established)) : claim?.kind === "configured" && claim.established;
  const validPort = /^\d+$/.test(port) && Number(port) >= 1 && Number(port) <= 65535;
  const now = Date.now() / 1000;
  const typed = domain.trim().toLowerCase().replace(/\.$/, "");
  const duplicate = inService.some(d => d.domain === typed);
  return <div className="config-section">
    <h3>Mail domain</h3>
    <p>Connect your mail domains to KyIdentity and use your own outgoing relay. You own the provider account, credentials, billing and delivery reputation.</p>
    <p>First configure <Link to="/admin/server?tab=sso">Single Sign-On (SSO)</Link> with KyIdentity. Existing IMAP accounts are never adopted into native mailboxes.</p>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {notice && <p className="notice" role="status">{notice}</p>}
    {!setup && <p>{error ? "Setup status unavailable. Reload this page before making changes." : "Loading mail setup…"}</p>}
    <fieldset className="config-card config-grid" disabled={busy || !setup}>
      <legend>Confirm each action</legend>
      {ssoSession ? <p>Confirm each action with KySignOn.</p> : <label>Account password<input type="password" autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} /></label>}
      <h4>1. Prove domain ownership</h4>
      {claim?.kind === "configured" ? <>
        <p>Domain: <strong>{claim.domain}</strong><br />Identity issuer: <code>{claim.issuer}</code></p>
        <p>{claim.established ? "Initial ownership established. Retain this TXT record; cached proof does not grant current authority." : `Initial challenge expires ${new Date(claim.expiresAt * 1000).toLocaleString()}.`}</p>
        <label>TXT name<input readOnly value={claim.recordName} /></label>
        <label>TXT value<textarea readOnly value={claim.recordValue} /></label>
        <div className="config-actions">
          <button className="button secondary" disabled={!unlocked} onClick={() => void act("verify")}>Verify TXT record</button>
          <button className="button secondary" disabled={!unlocked} onClick={() => void act("rotate")}>Replace challenge</button>
        </div>
        <p>This server supports one domain; domain and issuer are fixed. Replacing the challenge clears verification; update DNS and verify again.</p>
      </> : claim ? <>
        <label>Domain<input value={domain} placeholder="example.com" autoComplete="off" onChange={e => setDomain(e.target.value)} /></label>
        <p>Use a lowercase ASCII domain. This binds the domain to the configured identity issuer.</p>
        <button className="button secondary" disabled={!unlocked || !domain} onClick={() => void act("claim")}>Create TXT challenge</button>
      </> : <>
        {domainSet?.issuer && <p>Identity issuer: <code>{domainSet.issuer}</code>. Every domain is bound to this issuer.</p>}
        {domainSet && domainSet.domains.length === 0 && <p>No mail domains yet.</p>}
        {domainSet && domainSet.domains.length > 0 && <ul className="config-status-card" aria-label="Mail domains" style={{ listStyle: "none" }}>
          {domainSet.domains.map(d => {
            const status = domainStatus(d, now);
            const relayed = relayDomains.includes(d.domain);
            return <li key={d.domain} style={{ padding: "10px 0" }}>
              <div className="security-card-head">
                <h5>{d.domain}</h5>
                <span className={`security-badge ${d.established ? "security-badge-on" : "security-badge-off"}`}><span className="security-dot" aria-hidden="true" />{status.label}</span>
              </div>
              {d.domain === domainSet.founding && <p>Founding domain.</p>}
              <p>{status.detail}</p>
              {!d.retired && <>
                <label>TXT name<input readOnly value={d.recordName} /></label>
                <label>TXT value<textarea readOnly value={d.recordValue} /></label>
              </>}
              {relayed && <p>The relay sends for this domain; remove it from the relay first to retire it.</p>}
              <div className="config-actions">
                {d.retired
                  ? <button className="button secondary" aria-label={`Re-add ${d.domain}`} disabled={!unlocked} onClick={() => void act("claim", d.domain)}>Re-add</button>
                  : <>
                    <button className="button secondary" aria-label={`Verify ${d.domain}`} disabled={!unlocked} onClick={() => void act("verify", d.domain)}>Verify</button>
                    <button className="button secondary" aria-label={`Replace challenge for ${d.domain}`} disabled={!unlocked} onClick={() => void act("rotate", d.domain)}>Replace challenge</button>
                    <button className="button secondary" aria-label={`Retire ${d.domain}`} disabled={!unlocked || relayed} onClick={() => void act("retire", d.domain)}>Retire</button>
                  </>}
              </div>
            </li>;
          })}
        </ul>}
        {domainSet?.founding && <p>Older clients and the single-domain API use the founding domain. Retiring it hands that role to the alphabetically first remaining domain.</p>}
        <label>Domain<input value={domain} placeholder="example.com" autoComplete="off" onChange={e => setDomain(e.target.value)} /></label>
        {duplicate ? <p>{typed} is already configured; use its Replace challenge to rotate the TXT record.</p>
          : <p>Use a lowercase ASCII domain. Each domain gets its own TXT challenge under the same identity issuer.</p>}
        <button className="button secondary" disabled={!unlocked || !domain || duplicate} onClick={() => void act("claim", domain)}>Add domain</button>
      </>}
      <h4>2. Configure outgoing delivery</h4>
      {relay?.kind === "configured" && <p>Saved relay: {relay.host}:{relay.port}. {relay.sendingEnabled ? "Native primary sending is enabled." : "Native sending is disabled; enable KYPOST_NATIVE_MAIL in deployment to use it."} Provider delivery remains untested.</p>}
      <p>Use an authenticated implicit TLS endpoint, usually port 465. STARTTLS and plaintext endpoints are unsupported. Your provider must authorize the From addresses of every sending domain.</p>
      <p>KyPost holds this domain-wide credential on the server. A compromise exposing both its encrypted file and key exposes the relay account.</p>
      {domainSet && <fieldset>
        <legend>Relay sending domains</legend>
        {inService.map(d => <label key={d.domain} className="config-checkbox">
          <input type="checkbox" checked={selected.includes(d.domain)} disabled={!d.established && !relayDomains.includes(d.domain)}
            onChange={e => setSelected(e.target.checked ? [...selected, d.domain] : selected.filter(x => x !== d.domain))} />
          <span>{d.domain}{!d.established && " (not verified)"}</span>
        </label>)}
        {inService.length === 0 && <p>Add and verify a domain first.</p>}
        <p>Adding a domain needs a current DNS proof: KyPost checks its TXT record when you save. Removing a domain is refused while queued or retryable mail sends from it. A domain must leave the relay before it can be retired.</p>
        {relay?.kind === "configured" && relay.retiredDomains.length > 0 && <p>Previously sent for (kept to validate older queued mail): {relay.retiredDomains.join(", ")}</p>}
        {relay?.kind === "configured" && <>
          <p>Saving only the domains keeps the relay credentials and generation; no relay password is needed.</p>
          <button className="button secondary" disabled={!unlocked || selected.length === 0 || !domainsChanged} onClick={() => void act("domains")}>Save sending domains</button>
        </>}
      </fieldset>}
      <label>Relay host<input value={host} autoComplete="off" placeholder="smtp.provider.example" onChange={e => setHost(e.target.value)} /></label>
      <label>Relay port<input type="number" min={1} max={65535} value={port} onChange={e => setPort(e.target.value)} /></label>
      <label>Relay username<input value={username} autoComplete="off" onChange={e => setUsername(e.target.value)} /></label>
      <label>Relay password<input type="password" value={secret} autoComplete="off" onChange={e => setSecret(e.target.value)} /></label>
      <p>Every save replaces the complete relay profile. Credentials are never shown again and are cleared after each action.</p>
      {relay?.kind === "configured" && <p>Replacing this profile invalidates unclaimed queued deliveries. Check outbox and provider evidence before resubmitting; uncertain mail must never be blindly retried.</p>}
      <button className="button secondary" disabled={!unlocked || !canSave || !host || !username || !secret || !validPort} onClick={() => void act("relay")}>Save outgoing relay</button>
      {relay?.kind === "configured" && <>
        <p>Check saved relay contacts {relay.host}:{relay.port} using its stored credential. Unsaved edits above are not tested. The provider can record this login attempt; no email is sent.</p>
        <button className="button secondary" disabled={!unlocked || !canCheck} onClick={() => void act("check")}>Check saved relay</button>
      </>}
    </fieldset>
    {busy && <p role="status">Working…</p>}
    <h4>3. Back up and test</h4>
    <p>Configure <Link to="/admin/server?tab=backup">sealed backups</Link> and run a restore drill. Assign a new test identity in KyIdentity, then send from its primary address to a recipient you control. Provider acceptance does not prove inbox placement.</p>
    <p>Public receiving is not ready from this screen. A qualified receiving gateway must hold accepted mail until KyPost commits it. Complete controlled gateway, TLS, DNS and restore qualification before changing production MX.</p>
  </div>;
}
