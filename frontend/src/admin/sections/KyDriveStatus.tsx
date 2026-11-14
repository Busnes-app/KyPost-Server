import { useEffect, useState } from "react";
import { getJSON, toErrorMessage } from "../../api/client";

type Status = { source?: string; connected: boolean; url?: string; reason?: string };

/** How this server is paired with KyDrive. Read-only; the secret is never sent. */
export function KyDriveStatus() {
  const [status, setStatus] = useState<Status | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let live = true;
    getJSON<Status>("/api/admin/kydrive")
      .then(s => { if (live) setStatus(s); })
      .catch((e: unknown) => { if (live) setError(toErrorMessage(e, "Could not read the KyDrive status.")); });
    return () => { live = false; };
  }, []);

  return <div className="config-section">
    <h3>KyDrive</h3>
    {error && <p className="notice notice-error" role="alert">{error}</p>}
    {!status && !error && <p>Loading…</p>}
    {status?.connected && <>
      <p role="status">Connected via {status.source === "kyidentity" ? "KyIdentity" : "environment settings"} to {status.url}.</p>
      <p>Users who can open KyDrive can send large files as links and save attachments there. Each user controls this in KyDrive settings ("Let KyPost save and share my files").</p>
    </>}
    {status && !status.connected && <>
      <p role="status">Not connected{status.reason ? `: ${status.reason}` : ""}.</p>
      <p>KyPost pairs with KyDrive through KyIdentity on its own once Single Sign-On uses KyIdentity and KyDrive is registered there. KYPOST_KYDRIVE_URL and KYPOST_KYDRIVE_SECRET_FILE, when set, pair it instead.</p>
    </>}
  </div>;
}
