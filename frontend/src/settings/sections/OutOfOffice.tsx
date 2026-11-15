import { useEffect, useState } from "react";
import { toErrorMessage } from "../../api/client";
import {
  getOutOfOffice,
  MAX_OUT_OF_OFFICE_TEXT,
  putOutOfOffice,
  type OutOfOfficeView,
  type RemoteMode
} from "../../api/outOfOffice";
import { localDateTimeValue } from "../../lib/localDateTime";

const toInput = (iso: string) => (iso ? localDateTimeValue(new Date(iso)) : "");
const fromInput = (value: string) => (value ? new Date(value).toISOString() : "");
const browserZone = () => Intl.DateTimeFormat().resolvedOptions().timeZone || "";

/**
 * The automatic reply while the user is away: one text for people in the
 * organisation and, when the administrator allows it, one for everyone else.
 * The server decides who is who and answers each sender once.
 */
export function OutOfOffice() {
  const [settings, setSettings] = useState<OutOfOfficeView | null>(null);
  const [start, setStart] = useState("");
  const [end, setEnd] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);

  const load = (s: OutOfOfficeView) => {
    setSettings(s);
    setStart(toInput(s.start));
    setEnd(toInput(s.end));
  };

  useEffect(() => {
    let live = true;
    getOutOfOffice()
      .then((s) => { if (live) load(s); })
      .catch((error: unknown) => { if (live) setMessage(`Could not load your automatic reply: ${toErrorMessage(error, "unknown error")}`); });
    return () => { live = false; };
  }, []);

  if (!settings) {
    return (
      <div className="config-card">
        <h3>Out of office</h3>
        <p className="config-muted" role="status">{message || "Loading..."}</p>
      </div>
    );
  }

  const update = (patch: Partial<OutOfOfficeView>) => setSettings({ ...settings, ...patch });

  async function save() {
    if (!settings) return;
    setBusy(true);
    setMessage("");
    try {
      load(await putOutOfOffice({ ...settings, start: fromInput(start), end: fromInput(end), timeZone: browserZone() }));
      setMessage("Automatic reply saved.");
    } catch (error: unknown) {
      setMessage(`Could not save your automatic reply: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="config-card">
      <h3>Out of office</h3>
      <p className="config-muted">
        Sends an automatic reply while you are away. Each sender gets it once every four days at most, and never mailing
        lists, automated mail, junk or mail whose sender cannot be verified. Replies are saved in Sent.
      </p>
      <label className="config-checkbox">
        <input type="checkbox" checked={settings.enabled} onChange={(event) => update({ enabled: event.target.checked })} />
        <span>Send automatic replies</span>
      </label>
      <div className="config-grid config-grid-two">
        <label>
          <div>Start (optional)</div>
          <input type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} />
        </label>
        <label>
          <div>End (optional)</div>
          <input type="datetime-local" value={end} min={start || undefined} onChange={(event) => setEnd(event.target.value)} />
        </label>
      </div>
      <p className="config-muted">Times are in {settings.timeZone && settings.timeZone !== browserZone() ? `${browserZone()} (saved in ${settings.timeZone})` : browserZone() || "your local time"}.</p>
      <label>
        <div>Message for people in your organisation</div>
        <textarea
          className="config-textarea"
          rows={5}
          value={settings.localMessage}
          maxLength={MAX_OUT_OF_OFFICE_TEXT}
          onChange={(event) => update({ localMessage: event.target.value })}
        />
      </label>
      {settings.remoteAllowed ? (
        <>
          <label>
            <div>People outside your organisation</div>
            <select value={settings.remote} onChange={(event) => update({ remote: event.target.value as RemoteMode })}>
              <option value="off">Don't reply</option>
              <option value="contacts">Only my contacts</option>
              <option value="on">Everyone</option>
            </select>
          </label>
          {settings.remote !== "off" ? (
            <label>
              <div>Message for people outside your organisation</div>
              <textarea
                className="config-textarea"
                rows={5}
                value={settings.remoteMessage}
                maxLength={MAX_OUT_OF_OFFICE_TEXT}
                onChange={(event) => update({ remoteMessage: event.target.value })}
              />
            </label>
          ) : null}
        </>
      ) : (
        <p className="config-muted">Your administrator has not allowed automatic replies to people outside your organisation.</p>
      )}
      <div className="config-actions">
        <button type="button" className="btn-primary" onClick={() => void save()} disabled={busy}>
          {busy ? "Saving..." : "Save automatic reply"}
        </button>
      </div>
      <p className="config-muted" role="status">{message}</p>
    </div>
  );
}
