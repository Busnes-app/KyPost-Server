import { useEffect, useState } from "react";
import { getJSON, toErrorMessage } from "../../api/client";
import { usageLevel, usageNotice, usageText } from "../../lib/mailboxUsage";

type Mailbox = { id: string; addresses: { address: string }[]; usedBytes?: unknown; quotaBytes?: unknown };

const bytes = (v: unknown) => typeof v === "number" && Number.isSafeInteger(v) && v >= 0 ? v : null;

/** The user's own mailboxes against their quota, warning at 80% and 95%. */
export function MailStorage() {
  const [mailboxes, setMailboxes] = useState<Mailbox[] | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let live = true;
    getJSON<{ mailboxes: Mailbox[] }>("/api/mailboxes")
      .then(r => { if (live) setMailboxes(r.mailboxes); })
      .catch((e: unknown) => { if (live) { setMailboxes([]); setError(toErrorMessage(e, "Could not read your mailboxes.")); } });
    return () => { live = false; };
  }, []);

  if (mailboxes === null) return <div className="config-section"><h3>Storage</h3><p>Loading…</p></div>;
  return <div className="config-section">
    <h3>Storage</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {!error && mailboxes.length === 0 && <p>Storage is shown for mailboxes KyPost hosts. If your mail is with another provider, check there.</p>}
    {mailboxes.map(m => {
      const name = m.addresses[0]?.address ?? m.id;
      const used = bytes(m.usedBytes), quota = bytes(m.quotaBytes);
      if (used === null || !quota) return <p key={m.id}>{name}: usage unavailable right now.</p>;
      const notice = usageNotice(used, quota);
      return <div key={m.id} className="config-card">
        <label>{name}
          <meter min={0} max={quota} low={quota * 0.8} high={quota * 0.95} optimum={0} value={Math.min(used, quota)} aria-describedby={`usage-${m.id}`} />
        </label>
        <p id={`usage-${m.id}`}>{usageText(used, quota)}</p>
        {notice && <p className={`notice ${usageLevel(used, quota) === "critical" ? "notice-error" : "notice-warning"}`} role="note">{notice}</p>}
      </div>;
    })}
  </div>;
}
