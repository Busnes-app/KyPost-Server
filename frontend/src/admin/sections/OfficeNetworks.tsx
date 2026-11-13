import { FormEvent, useEffect, useState } from "react";
import { getJSON, putJSON, toErrorMessage } from "../../api/client";
import { confirmedAdmin } from "../../api/users";
import { useAuth } from "../../auth";

type ExemptStatus = { networks: string; environment: string; exempted: number };

const ROUTE = "/api/admin/rate-limit-networks";

const lines = (list: string) => list.split(",").filter(Boolean).join("\n");

/** Office networks whose sign-ins skip the per-address rate limits. One network per line. */
export function OfficeNetworks() {
  const ssoSession = useAuth().ssoSession === true;
  const [status, setStatus] = useState<ExemptStatus | null>(null);
  const [text, setText] = useState("");
  const [password, setPassword] = useState("");
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  const show = (s: ExemptStatus) => {
    setStatus(s);
    setText(lines(s.networks));
  };

  useEffect(() => {
    getJSON<ExemptStatus>(ROUTE)
      .then(show)
      .catch((err) => setError(toErrorMessage(err, "Could not load the networks.")));
  }, []);

  async function save(e: FormEvent) {
    e.preventDefault();
    setSaving(true);
    setMessage("");
    setError("");
    const accountPassword = password;
    setPassword("");
    try {
      const networks = text.split(/[\s,]+/).filter(Boolean).join(",");
      const saved = await confirmedAdmin({ ssoSession, accountPassword }, { networks }, (b, h) => putJSON<ExemptStatus>(ROUTE, b, h));
      show(saved);
      setMessage(saved.networks ? "Saved. These networks now skip the per-address limit." : "Saved. No network skips the limit.");
    } catch (err) {
      setError(toErrorMessage(err, "Could not save the networks."));
    } finally {
      setSaving(false);
    }
  }

  return (
    <form onSubmit={save} className="config-section">
      <div className="section-head">
        <h3>Office networks</h3>
        <p className="muted">
          Sign-in is limited per network address. Staff in one office usually share one address and can trip that limit together.
          Networks listed here skip it; per-account limits and the security check still apply.
        </p>
      </div>

      <div className="field-group">
        <label htmlFor="exempt_networks">Networks that skip the per-address limit, one per line</label>
        <textarea
          id="exempt_networks"
          className="input font-mono"
          rows={4}
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder="203.0.113.0/24"
          aria-describedby="exempt_networks_hint"
          disabled={!status}
        />
        <span id="exempt_networks_hint" className="muted" style={{ fontSize: "0.75rem", marginTop: "0.25rem", display: "block" }}>
          List only networks you control: everyone behind a listed address loses the limit, so never add guest Wi-Fi, a VPN
          provider or your reverse proxy. The widest range accepted is /16 (IPv4) or /48 (IPv6).
        </span>
      </div>

      {status ? (
        <dl className="muted">
          {status.environment ? (
            <>
              <dt>Also set by the server environment</dt>
              <dd className="font-mono">{status.environment.split(",").join(", ")}</dd>
            </>
          ) : null}
          <dt>Requests that skipped the limit</dt>
          <dd>{status.exempted} since the last change or restart</dd>
        </dl>
      ) : null}

      {message ? <p className="notice" role="status">{message}</p> : null}
      {error ? <p className="notice notice-error" role="alert">{error}</p> : null}

      <div className="field-group">
        {ssoSession ? (
          <p className="muted">Saving asks you to confirm with KySignOn.</p>
        ) : (
          <>
            <label htmlFor="exempt_confirm_password">Your account password</label>
            <input
              id="exempt_confirm_password"
              type="password"
              className="input"
              autoComplete="current-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </>
        )}
      </div>

      <div className="actions" style={{ marginTop: "1rem" }}>
        <button type="submit" className="button" disabled={saving || !status || (!ssoSession && !password)}>
          {saving ? "Saving…" : "Save networks"}
        </button>
      </div>
    </form>
  );
}
