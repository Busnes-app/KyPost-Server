import { useEffect, useState } from "react";
import { getJSON, toErrorMessage } from "../../api/client";

type Status = { source?: string; connected: boolean; url?: string; reason?: string };

/** How this server is paired with KyCalendar. Read-only; the secret is never sent. */
export function KyCalendarStatus() {
  const [status, setStatus] = useState<Status | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let live = true;
    getJSON<Status>("/api/admin/kycalendar")
      .then(s => { if (live) setStatus(s); })
      .catch((e: unknown) => { if (live) setError(toErrorMessage(e, "Could not read the KyCalendar status.")); });
    return () => { live = false; };
  }, []);

  return <div className="config-section">
    <h3>KyCalendar</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {!status && !error && <p>Loading…</p>}
    {status?.connected && status.source === "kyidentity" && <>
      <p role="status">Connected via KyIdentity to {status.url}.</p>
      <p>Invitation mail is on for every user who signs in through KyIdentity, unless they turn it off under Settings → Mail → KyCalendar.</p>
    </>}
    {status?.connected && status.source !== "kyidentity" && <>
      <p role="status">Connected via environment settings to {status.url}.</p>
      <p>KYPOST_KYCALENDAR_URL and KYPOST_KYCALENDAR_SECRET_FILE pair this server. Each user turns invitation mail on under Settings → Mail → KyCalendar.</p>
    </>}
    {status && !status.connected && <>
      <p role="status">Not connected{status.reason ? `: ${status.reason}` : ""}.</p>
      <p>KyPost pairs with KyCalendar through KyIdentity on its own once Single Sign-On uses KyIdentity and KyCalendar is registered there. KYPOST_KYCALENDAR_URL and KYPOST_KYCALENDAR_SECRET_FILE, when set, pair it instead.</p>
    </>}
  </div>;
}
