import { useState } from "react";
import { driveErrorCode, openInDrive } from "../../api/kydrive";
import { toErrorMessage } from "../../api/client";

const refusals: Record<string, string> = {
  too_large: "This file is larger than KyDrive accepts.",
  quota: "Your KyDrive is full.",
  sharing_off: "KyPost is not allowed to use your KyDrive. Turn it on in KyDrive settings.",
  kydrive_off: "KyDrive is not available for your account.",
  encrypted: "Attachments of encrypted mail stay on your device.",
  unsupported_type: "This attachment cannot be opened in KyDrive.",
  editor_off: "KyDrive has no document viewer.",
  too_many_tickets: "Close another opened attachment first."
};

/**
 * Open (read-only, a 24-hour viewing copy) and Edit in KyDrive (a saved copy)
 * for one received Office or PDF attachment. The reader renders it only for
 * a viewable type of an unencrypted message; the server checks both again.
 */
export function OfficeActions({ messageId, folder, index, name, origin }: { messageId: string; folder: string; index: number; name: string; origin: string }) {
  const [busy, setBusy] = useState<"" | "view" | "edit">("");
  const [error, setError] = useState("");
  const run = (mode: "view" | "edit") => {
    setBusy(mode);
    setError("");
    openInDrive(mode, messageId, folder, index, origin)
      .catch((e: unknown) => setError(refusals[driveErrorCode(e)] ?? toErrorMessage(e, "Could not open it in KyDrive.")))
      .finally(() => setBusy(""));
  };
  return (
    <span className="email-attachment-drive">
      <button type="button" className="btn-ghost" aria-label={`Open ${name} read-only`} disabled={busy !== ""} onClick={() => run("view")}>
        {busy === "view" ? "Opening…" : "Open"}
      </button>
      <button type="button" className="btn-ghost" aria-label={`Edit ${name} in KyDrive`} disabled={busy !== ""} onClick={() => run("edit")}>
        {busy === "edit" ? "Saving to KyDrive…" : "Edit in KyDrive"}
      </button>
      {error ? <span className="email-attachments-error" role="alert"> {error}</span> : null}
    </span>
  );
}
