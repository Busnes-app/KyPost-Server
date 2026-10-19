import { useEffect, useState } from "react";
import { getJSON, putJSON, toErrorMessage } from "../../api/client";

type State = { available: boolean; enabled: boolean };

/** The user's opt-in for KyCalendar sending and receiving invitations. */
export function KyCalendar() {
  const [state, setState] = useState<State | null>(null);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let live = true;
    getJSON<State>("/api/settings/kycalendar")
      .then(s => { if (live) setState(s); })
      .catch((e: unknown) => { if (live) setError(toErrorMessage(e, "Could not read your KyCalendar setting.")); });
    return () => { live = false; };
  }, []);

  const save = (enabled: boolean) => {
    setSaving(true);
    setError("");
    putJSON<State>("/api/settings/kycalendar", { enabled })
      .then(setState)
      .catch((e: unknown) => setError(toErrorMessage(e, "Could not save your KyCalendar setting.")))
      .finally(() => setSaving(false));
  };

  return <div className="config-section">
    <h3>KyCalendar</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {!state && !error && <p>Loading…</p>}
    {state && !state.available && <p>This server is not paired with KyCalendar.</p>}
    {state?.available && <>
      <label>
        <input type="checkbox" checked={state.enabled} disabled={saving} onChange={e => save(e.target.checked)} />
        {" "}Let KyCalendar send invitations from this mailbox
      </label>
      <p>KyCalendar can then mail invitations, cancellations and replies as you, through your own mail server, to the people each event names, and receives the signed invitations you get here.</p>
    </>}
  </div>;
}
