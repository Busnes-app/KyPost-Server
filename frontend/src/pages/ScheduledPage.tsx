import { useEffect, useState } from "react";
import { selectedMailboxId, toErrorMessage } from "../api/client";
import { cancelScheduled, listScheduled, type ScheduledMessage } from "../api/scheduled";
import { OUTER_PLACEHOLDER_SUBJECT } from "../lib/pgpClient";

const STATUS: Record<string, string> = {
  scheduled: "Waiting",
  sending: "Sending",
  returning: "Returning to Drafts",
  returned: "Returned to Drafts",
  sent: "Sent",
  queued: "Handed to the outbox",
  uncertain: "Not confirmed",
  failed: "Failed"
};

/** The selected mailbox's scheduled mail, with Cancel for what has not started sending. */
export function ScheduledPage() {
  const mailbox = selectedMailboxId();
  const [rows, setRows] = useState<ScheduledMessage[] | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [version, setVersion] = useState(0);

  useEffect(() => {
    listScheduled(mailbox)
      .then(setRows)
      .catch((e) => setError(toErrorMessage(e, "could not load scheduled mail")));
  }, [mailbox, version]);

  async function cancel(id: string) {
    setBusy(id);
    try {
      const result = await cancelScheduled(mailbox, id);
      setError(result.warning ?? "");
    } catch (e) {
      setError(toErrorMessage(e, "could not cancel"));
    } finally {
      setBusy("");
      setVersion((v) => v + 1);
    }
  }

  return (
    <section className="panel config-page">
      <div className="config-header">
        <h1>Scheduled</h1>
        <p>Mail waiting to send. Canceled or unsendable mail returns to Drafts.</p>
      </div>
      {error ? <p className="notice notice-error">{error}</p> : null}
      {rows === null ? <p>Loading&hellip;</p> : rows.length === 0 ? <p className="config-muted">Nothing is scheduled.</p> : (
        <table className="config-table">
          <thead>
            <tr><th>Send time</th><th>To</th><th>Subject</th><th>Status</th><th /></tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.id}>
                <td>{new Date(row.sendAt).toLocaleString()}</td>
                <td>{row.to.join(", ")}</td>
                <td>{row.subject === OUTER_PLACEHOLDER_SUBJECT ? "(encrypted)" : row.subject}</td>
                <td>
                  {STATUS[row.status] ?? row.status}
                  {row.reason ? <div className="config-muted">{row.reason}</div> : null}
                </td>
                <td>
                  {row.status === "scheduled" ? (
                    <button type="button" className="button" disabled={busy !== ""} onClick={() => void cancel(row.id)}>Cancel</button>
                  ) : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}
