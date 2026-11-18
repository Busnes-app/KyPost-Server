import { FormEvent, useEffect, useState } from "react";
import { HttpError, getJSON, putJSON, toErrorMessage } from "../../api/client";
import { readMailDomains } from "../../api/nativeMail";
import { confirmedAdmin } from "../../api/users";
import { useAuth } from "../../auth";

const ROUTE = "/api/admin/masked-domains";

function readDomains(value: unknown): string[] {
  const domains = (value as { domains?: unknown } | null)?.domains;
  if (!Array.isArray(domains) || !domains.every((d) => typeof d === "string")) throw new Error("invalid masked domain list");
  return domains;
}

/** The domains users may make masked and disposable addresses on. None chosen keeps the feature off. */
export function MaskedDomains() {
  const ssoSession = useAuth().ssoSession === true;
  const [configured, setConfigured] = useState<string[] | null>(null);
  const [chosen, setChosen] = useState<string[]>([]);
  const [unavailable, setUnavailable] = useState(false);
  const [password, setPassword] = useState("");
  const [saving, setSaving] = useState(false);
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");

  useEffect(() => {
    Promise.all([getJSON<unknown>(ROUTE), getJSON<unknown>("/api/admin/mail-domains")])
      .then(([masked, set]) => {
        setChosen(readDomains(masked));
        setConfigured(readMailDomains(set).domains.filter((d) => !d.retired).map((d) => d.domain));
      })
      .catch((err) => {
        if (err instanceof HttpError && err.status === 404) setUnavailable(true);
        else setError(toErrorMessage(err, "Could not load the masked address domains."));
      });
  }, []);

  async function save(e: FormEvent) {
    e.preventDefault();
    setSaving(true);
    setMessage("");
    setError("");
    const accountPassword = password;
    setPassword("");
    try {
      const saved = readDomains(await confirmedAdmin({ ssoSession, accountPassword }, { domains: chosen }, (b, h) => putJSON<unknown>(ROUTE, b, h)));
      setChosen(saved);
      setMessage(saved.length ? "Saved. Users can make masked addresses on these domains." : "Saved. Masked addresses are off; existing ones keep working until their owners delete them.");
    } catch (err) {
      setError(toErrorMessage(err, "Could not save the masked address domains."));
    } finally {
      setSaving(false);
    }
  }

  if (unavailable) return null;
  return (
    <form onSubmit={save} className="config-section">
      <div className="section-head">
        <h3>Masked addresses</h3>
        <p className="muted">
          Users can make their own random masked and disposable addresses on the domains you tick, up to 100 each and 20 a
          day. Their mail counts toward the owner's sending limits. Deleted addresses stay reserved and are never reissued.
          Nothing is ticked by default, which keeps the feature off.
        </p>
      </div>
      {configured?.length === 0 ? <p className="muted">Add a mail domain first.</p> : null}
      {configured?.map((d) => (
        <label key={d} className="security-check">
          <input type="checkbox" checked={chosen.includes(d)} disabled={saving}
            onChange={(e) => setChosen(e.target.checked ? [...chosen, d].sort() : chosen.filter((x) => x !== d))} />
          {" "}{d}
        </label>
      ))}
      {message ? <p className="notice" role="status">{message}</p> : null}
      {error ? <p className="notice notice-error" role="alert">{error}</p> : null}
      <div className="field-group">
        {ssoSession ? (
          <p className="muted">Saving asks you to confirm with KySignOn.</p>
        ) : (
          <>
            <label htmlFor="masked_confirm_password">Your account password</label>
            <input id="masked_confirm_password" type="password" className="input" autoComplete="current-password"
              value={password} onChange={(e) => setPassword(e.target.value)} />
          </>
        )}
      </div>
      <div className="actions" style={{ marginTop: "1rem" }}>
        <button type="submit" className="button" disabled={saving || !configured || (!ssoSession && !password)}>
          {saving ? "Saving…" : "Save masked domains"}
        </button>
      </div>
    </form>
  );
}
