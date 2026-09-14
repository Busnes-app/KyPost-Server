import { useEffect, useState } from "react";
import { toErrorMessage } from "../../api/client";
import { getMailDefaults, putMailDefaults, type MailDefaults as Defaults } from "../../api/mailDefaults";

const EMPTY: Defaults = { host: "", port: 993, smtpHost: "", smtpPort: 587 };

export function MailDefaults() {
  const [form, setForm] = useState<Defaults>(EMPTY);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");

  useEffect(() => {
    getMailDefaults()
      .then((d) => setForm({ host: d.host, port: d.port || 993, smtpHost: d.smtpHost, smtpPort: d.smtpPort || 587 }))
      .catch(() => undefined);
  }, []);

  async function save() {
    setBusy(true);
    setMessage("");
    try {
      await putMailDefaults(form);
      setMessage("Defaults saved. New users will see these values prefilled in Email Settings.");
    } catch (error: unknown) {
      setMessage(`Failed to save defaults: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBusy(false);
    }
  }

  const tone = message.startsWith("Failed") ? "notice notice-error" : "notice notice-success";

  return (
    <div className="config-card">
      <h3>Default Mail Server</h3>
      <p className="config-muted">
        Prefills the Email Settings form for users who have not connected a mailbox yet. Users still enter their own
        username and password. Leave blank to prefill nothing.
      </p>
      <div className="config-grid config-grid-two">
        <label>
          <div>IMAP Host</div>
          <input value={form.host} onChange={(e) => setForm((p) => ({ ...p, host: e.target.value }))} />
        </label>
        <label>
          <div>IMAP Port</div>
          <input type="number" value={form.port} onChange={(e) => setForm((p) => ({ ...p, port: Number(e.target.value) || 993 }))} />
        </label>
        <label>
          <div>SMTP Host</div>
          <input value={form.smtpHost} onChange={(e) => setForm((p) => ({ ...p, smtpHost: e.target.value }))} />
        </label>
        <label>
          <div>SMTP Port</div>
          <input type="number" value={form.smtpPort} onChange={(e) => setForm((p) => ({ ...p, smtpPort: Number(e.target.value) || 587 }))} />
        </label>
      </div>
      <div className="config-actions">
        <button type="button" onClick={() => void save()} disabled={busy}>
          {busy ? "Saving..." : "Save Defaults"}
        </button>
      </div>
      {message ? <p className={tone}>{message}</p> : null}
    </div>
  );
}
