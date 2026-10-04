import { useEffect, useRef, useState } from "react";
import { Link } from "react-router";
import { useAuth } from "../../auth";
import { credentialFields, deriveCredential } from "../../api/auth";
import { postJSON, putJSON, toErrorMessage } from "../../api/client";
import { loadNativeMail, readMailRelayCheck } from "../../api/nativeMail";
import { withSSOStepUp } from "../../api/stepup";

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
  const [setup, setSetup] = useState<Awaited<ReturnType<typeof loadNativeMail>> | null>(null);
  const [error, setError] = useState("");
  const [notice, setNotice] = useState("");
  const [busy, setBusy] = useState(false);
  const [domain, setDomain] = useState("");
  const [host, setHost] = useState("");
  const [port, setPort] = useState("465");
  const [username, setUsername] = useState("");
  const [secret, setSecret] = useState("");
  const [password, setPassword] = useState("");
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
  async function act(action: "claim" | "rotate" | "verify" | "relay" | "check") {
    if (!setup || inFlight.current || action === "check" && setup.relay.kind !== "configured") return;
    if (action === "rotate" && !window.confirm("Replace the TXT challenge? This clears domain verification and pauses native account allocation and sending until the new record is verified.")) return;
    if (action === "relay" && setup.relay.kind === "configured" && !window.confirm("Replace the relay profile? This rotates its generation and stops unclaimed deliveries queued under the old profile. Check outbox and provider evidence before resubmitting to avoid duplicates.")) return;
    inFlight.current = true;
    setBusy(true); setError(""); setNotice("");
    // Freeze the request before credential derivation or KySignOn confirmation.
    const fields = action === "relay"
      ? { host, port: Number(port), smtpUsername: username, smtpPassword: secret }
      : action === "check" && setup.relay.kind === "configured" ? { expectedGeneration: setup.relay.generation }
      : action === "verify" ? {} : { domain: setup.domain.kind === "configured" ? setup.domain.domain : domain };
    const accountPassword = password;
    setPassword(""); setSecret(""); setUsername("");
    try {
      const credential = ssoSession ? {} : credentialFields(await deriveCredential("", accountPassword));
      requireLive();
      const body = { ...fields, ...credential };
      const result = await withSSOStepUp((headers) => {
        requireLive();
        return action === "check"
          ? postJSON<unknown>("/api/admin/mail-relay/test", body, headers)
          : action === "verify"
          ? postJSON<unknown>("/api/admin/mail-domain/verify", body, headers)
          : putJSON<unknown>(action === "relay" ? "/api/admin/mail-relay" : "/api/admin/mail-domain", body, headers);
      });
      requireLive();
      if (action === "check") readMailRelayCheck(result, setup.relay);
      // A successful mutation followed by an unreadable GET must not leave stale controls enabled.
      setSetup(null);
      const refreshed = await refresh();
      requireLive();
      if (action === "check" && refreshed) readMailRelayCheck(result, refreshed.relay);
      setNotice(action === "check" ? "TLS and SMTP authentication passed for the saved relay. No email was sent; delivery remains untested." : action === "relay" ? "Relay saved. No provider connection or test email was made." : action === "verify" ? "TXT record matched. KyPost rechecks DNS before native operations." : "Challenge created. Publish the exact TXT record below, then verify it.");
    } catch (e: unknown) {
      if (live.current) {
        setSetup(null);
        setError(toErrorMessage(e, "Mail setup failed. Reload status before retrying an uncertain change."));
      }
    } finally {
      inFlight.current = false;
      if (live.current) { setPassword(""); setSecret(""); setUsername(""); setBusy(false); }
    }
  }
  const unlocked = ssoSession || password.length > 0;
  const claim = setup?.domain;
  const relay = setup?.relay;
  const validPort = /^\d+$/.test(port) && Number(port) >= 1 && Number(port) <= 65535;
  return <div className="config-section">
    <h3>Mail domain</h3>
    <p>Connect one domain to KyIdentity and use your own outgoing relay. You own the provider account, credentials, billing and delivery reputation.</p>
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
        <p>Domain and issuer are fixed for this stack. Replacing the challenge clears verification; update DNS and verify again.</p>
      </> : <>
        <label>Domain<input value={domain} placeholder="example.com" autoComplete="off" onChange={e => setDomain(e.target.value)} /></label>
        <p>Use a lowercase ASCII domain. This binds the domain to the configured identity issuer.</p>
        <button className="button secondary" disabled={!unlocked || !domain} onClick={() => void act("claim")}>Create TXT challenge</button>
      </>}
      <h4>2. Configure outgoing delivery</h4>
      {relay?.kind === "configured" && <p>Saved relay: {relay.host}:{relay.port}. {relay.sendingEnabled ? "Native primary sending is enabled." : "Native sending is disabled; enable KYPOST_NATIVE_MAIL in deployment to use it."} Provider delivery remains untested.</p>}
      <p>Use an authenticated implicit TLS endpoint, usually port 465. STARTTLS and plaintext endpoints are unsupported. Your provider must authorize this domain’s From addresses.</p>
      <p>KyPost holds this domain-wide credential on the server. A compromise exposing both its encrypted file and key exposes the relay account.</p>
      <label>Relay host<input value={host} autoComplete="off" placeholder="smtp.provider.example" onChange={e => setHost(e.target.value)} /></label>
      <label>Relay port<input type="number" min={1} max={65535} value={port} onChange={e => setPort(e.target.value)} /></label>
      <label>Relay username<input value={username} autoComplete="off" onChange={e => setUsername(e.target.value)} /></label>
      <label>Relay password<input type="password" value={secret} autoComplete="off" onChange={e => setSecret(e.target.value)} /></label>
      <p>Every save replaces the complete relay profile. Credentials are never shown again and are cleared after each action.</p>
      {relay?.kind === "configured" && <p>Replacing this profile invalidates unclaimed queued deliveries. Check outbox and provider evidence before resubmitting; uncertain mail must never be blindly retried.</p>}
      <button className="button secondary" disabled={!unlocked || claim?.kind !== "configured" || !claim.established || !host || !username || !secret || !validPort} onClick={() => void act("relay")}>Save outgoing relay</button>
      {relay?.kind === "configured" && <>
        <p>Check saved relay contacts {relay.host}:{relay.port} using its stored credential. Unsaved edits above are not tested. The provider can record this login attempt; no email is sent.</p>
        <button className="button secondary" disabled={!unlocked || claim?.kind !== "configured" || !claim.established} onClick={() => void act("check")}>Check saved relay</button>
      </>}
    </fieldset>
    {busy && <p role="status">Working…</p>}
    <h4>3. Back up and test</h4>
    <p>Configure <Link to="/admin/server?tab=backup">sealed backups</Link> and run a restore drill. Assign a new test identity in KyIdentity, then send from its primary address to a recipient you control. Provider acceptance does not prove inbox placement.</p>
    <p>Public receiving is not ready from this screen. A qualified receiving gateway must hold accepted mail until KyPost commits it. Complete controlled gateway, TLS, DNS and restore qualification before changing production MX.</p>
  </div>;
}
