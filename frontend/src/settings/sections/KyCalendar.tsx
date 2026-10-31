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
    {state && !state.available && <p>Invitation mail with KyCalendar is not connected on this server. Your administrator can see why under Server → KyCalendar. It lets KyCalendar send and receive meeting invitations through your mailbox; it does not sync your calendars.</p>}
    {state?.available && <>
      <label>
        <input type="checkbox" checked={state.enabled} disabled={saving} onChange={e => save(e.target.checked)} />
        {" "}Let KyCalendar send invitations from this mailbox
      </label>
      <p>KyCalendar can then mail invitations, cancellations and replies as you, through your own mail server, to the people each event names, and receives the signed invitations you get here. When your server pairs KyCalendar through KyIdentity, this is on until you turn it off.</p>
    </>}
  </div>;
}
