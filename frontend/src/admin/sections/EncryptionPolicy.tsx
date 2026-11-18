import { FormEvent, useEffect, useState } from "react";
import { getJSON, putJSON, toErrorMessage } from "../../api/client";
import { confirmedAdmin } from "../../api/users";
import { useAuth } from "../../auth";

type KeyPolicy = "required" | "optional";
type IncomingPolicy = "required" | "on" | "off";
type PolicyView = { key: KeyPolicy; incoming: IncomingPolicy; genericProvider: boolean; issuer?: string };

const ROUTE = "/api/admin/encryption-policy";

const BODY_WARNING =
  "Server search and body filter (Sieve) rules will only be able to use headers once mail is encrypted; AI labels are applied as mail arrives, before it is encrypted. " +
  "Only new mail is encrypted: unread inbox mail KyPost has not processed yet. Existing mail is not swept. Save anyway?";

function providerName(issuer?: string): string {
  try {
    return issuer ? new URL(issuer).host : "your sign-in provider";
  } catch {
    return issuer ?? "your sign-in provider";
  }
}

/** Server → Security: the first sign-in encryption policy. Admins never see or hold a key. */
export function EncryptionPolicy() {
  const ssoSession = useAuth().ssoSession === true;
  const [view, setView] = useState<PolicyView | null>(null);
  const [key, setKey] = useState<KeyPolicy>("required");
  const [incoming, setIncoming] = useState<IncomingPolicy>("on");
  const [password, setPassword] = useState("");
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  const show = (v: PolicyView) => {
    setView(v);
    setKey(v.key);
    setIncoming(v.incoming);
  };
  useEffect(() => {
    getJSON<PolicyView>(ROUTE)
      .then(show)
      .catch((err) => setError(toErrorMessage(err, "Could not load the encryption policy.")));
  }, []);

  const generic = view?.genericProvider === true;
  const needsKySignOn = `Needs KySignOn: users signing in through ${providerName(view?.issuer)} couldn't create a key and would be locked out.`;

  async function save(e: FormEvent) {
    e.preventDefault();
    if (incoming === "required" && view?.incoming !== "required" && !window.confirm(BODY_WARNING)) return;
    setSaving(true);
    setMessage("");
    setError("");
    const accountPassword = password;
    setPassword("");
    try {
      const saved = await confirmedAdmin({ ssoSession, accountPassword }, { key, incoming }, (b, h) => putJSON<PolicyView>(ROUTE, b, h));
      show(saved);
      setMessage("Saved. Users without a key are sent through setup at their next sign-in when it is required.");
    } catch (err) {
      setError(toErrorMessage(err, "Could not save the encryption policy."));
    } finally {
      setSaving(false);
    }
  }

  return (
    <form onSubmit={save} className="config-section">
      <div className="section-head">
        <h3>Email encryption</h3>
        <p className="muted">
          Every user creates or imports their own key at first sign-in, in their browser. You can require it, but you can never
          generate, see or hold anyone&apos;s key. Changes here are recorded in the audit log.
        </p>
      </div>

      {generic && view && (view.key === "required" || view.incoming === "required") ? (
        <p className="notice notice-error" role="alert">
          {needsKySignOn} They are skipped; lower these settings to Optional.
        </p>
      ) : null}

      <div className="field-group">
        <label htmlFor="policy_key">Encryption key</label>
        <select id="policy_key" className="input" value={key} disabled={!view || incoming === "required"} onChange={(e) => setKey(e.target.value === "optional" ? "optional" : "required")}>
          <option value="required" disabled={generic}>Required</option>
          <option value="optional">Optional</option>
        </select>
        <span className="muted">
          {key === "required"
            ? "Required: users without a key, existing ones included, set one up at their next sign-in, and their mail stays locked until it's done."
            : "Optional: the same step is offered at first sign-in, with “Not now”."}
        </span>
        {generic ? <span className="muted">{needsKySignOn}</span> : null}
      </div>

      <div className="field-group">
        <label htmlFor="policy_incoming">Encrypt incoming mail</label>
        <select
          id="policy_incoming"
          className="input"
          value={incoming}
          disabled={!view}
          onChange={(e) => {
            const next = e.target.value === "required" ? "required" : e.target.value === "off" ? "off" : "on";
            setIncoming(next);
            if (next === "required") setKey("required");
          }}
        >
          <option value="required" disabled={generic}>Required</option>
          <option value="on">On by default</option>
          <option value="off">Off by default</option>
        </select>
        <span className="muted">
          {incoming === "required"
            ? "Required: every user's incoming mail is encrypted to their key on arrival, and they cannot turn it off. This also sets Encryption key to Required: mail can't be encrypted to a key that doesn't exist."
            : "Sets the choice the first sign-in step starts with. Users can change it later in Security → Encryption."}
        </span>
        <span className="muted">
          Only new mail is encrypted: unread inbox mail KyPost has not processed yet. Turning on Required doesn&apos;t sweep existing mail.
        </span>
      </div>

      {message ? <p className="notice" role="status">{message}</p> : null}
      {error ? <p className="notice notice-error" role="alert">{error}</p> : null}

      <div className="field-group">
        {ssoSession ? (
          <p className="muted">Saving asks you to confirm with KySignOn.</p>
        ) : (
          <>
            <label htmlFor="policy_confirm_password">Your account password</label>
            <input id="policy_confirm_password" type="password" className="input" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
          </>
        )}
      </div>

      <div className="actions">
        <button type="submit" className="button" disabled={saving || !view || (!ssoSession && !password)}>
          {saving ? "Saving…" : "Save encryption policy"}
        </button>
      </div>
    </form>
  );
}
