import { useEffect, useState } from "react";
import { getJSON, putJSON, toErrorMessage } from "../../api/client";

export type MailSettings = { conversations: boolean };

/** "Group messages into conversations", stored per user on the server (GET|PUT /api/settings/mail). */
export function Conversations() {
  const [state, setState] = useState<MailSettings | null>(null);
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);

  useEffect(() => {
    let live = true;
    getJSON<MailSettings>("/api/settings/mail")
      .then((s) => { if (live) setState(s); })
      .catch((e: unknown) => { if (live) setError(toErrorMessage(e, "Could not read your conversation setting.")); });
    return () => { live = false; };
  }, []);

  const save = (conversations: boolean) => {
    setSaving(true);
    setError("");
    putJSON<MailSettings>("/api/settings/mail", { conversations })
      .then(setState)
      .catch((e: unknown) => setError(toErrorMessage(e, "Could not save your conversation setting.")))
      .finally(() => setSaving(false));
  };

  return <div className="config-section">
    <h3>Conversations</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {!state && !error && <p>Loading…</p>}
    {state && <>
      <label>
        <input type="checkbox" checked={state.conversations} disabled={saving} onChange={(e) => save(e.target.checked)} />
        {" "}Group messages into conversations
      </label>
      <p>Replies to the same message share one row in the list, with your own replies from Sent shown in place. Actions from the list then apply to the whole conversation; inside the reader they apply to the open message. Saved to your account, not this browser.</p>
    </>}
  </div>;
}
