import { useEffect, useState } from "react";
import { getJSON, toErrorMessage } from "../../api/client";

type Status = { source?: string; connected: boolean; url?: string; reason?: string };

/** How this server is paired with the KyMessages console. Read-only; the secret is never sent. */
export function KyMessagesStatus() {
  const [status, setStatus] = useState<Status | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let live = true;
    getJSON<Status>("/api/admin/kymessage")
      .then(s => { if (live) setStatus(s); })
      .catch((e: unknown) => { if (live) setError(toErrorMessage(e, "Could not read the KyMessages status.")); });
    return () => { live = false; };
  }, []);

  return <div className="config-section">
    <h3>KyMessages</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {!status && !error && <p>Loading…</p>}
    {status?.connected && <>
      <p role="status">Connected via {status.source === "kyidentity" ? "KyIdentity" : "environment settings"} to {status.url}.</p>
      <p>Users who can open KyMessages see Chat beside senders and contacts in the organisation. It opens their direct chat in KyMessages; KyPost creates nothing there.</p>
    </>}
    {status && !status.connected && <>
      <p role="status">Not connected{status.reason ? `: ${status.reason}` : ""}.</p>
      <p>KyPost pairs with KyMessages through KyIdentity on its own once Single Sign-On uses KyIdentity and KyMessages is registered there. KYPOST_KYMESSAGE_URL and KYPOST_KYMESSAGE_SECRET_FILE, when set, pair it instead.</p>
    </>}
  </div>;
}
