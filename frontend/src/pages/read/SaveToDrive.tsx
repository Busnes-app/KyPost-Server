import { useState } from "react";
import { saveAttachmentToDrive, driveErrorCode, type DriveSaved } from "../../api/kydrive";
import { toErrorMessage } from "../../api/client";

const refusals: Record<string, string> = {
  too_large: "This file is larger than KyDrive accepts.",
  quota: "Your KyDrive is full.",
  sharing_off: "KyPost is not allowed to save to your KyDrive. Turn it on in KyDrive settings.",
  kydrive_off: "KyDrive is not available for your account."
};

/** "Save to KyDrive" for one received attachment; KyPost's server copies the part. */
export function SaveToDrive({ messageId, folder, index, name }: { messageId: string; folder: string; index: number; name: string }) {
  const [state, setState] = useState<"idle" | "saving" | DriveSaved>("idle");
  const [error, setError] = useState("");
  if (typeof state === "object") {
    return (
      <span className="email-attachment-drive" role="status">
        Saved to KyDrive in {state.folder} as {state.name}.{" "}
        {state.appUrl?.startsWith("https://") ? <a href={state.appUrl} target="_blank" rel="noopener noreferrer">Open KyDrive</a> : null}
      </span>
    );
  }
  return (
    <span className="email-attachment-drive">
      <button
        type="button"
        className="btn-ghost"
        aria-label={`Save ${name} to KyDrive`}
        disabled={state === "saving"}
        onClick={() => {
          setState("saving");
          setError("");
          saveAttachmentToDrive(messageId, folder, index)
            .then(setState)
            .catch((e: unknown) => {
              setState("idle");
              setError(refusals[driveErrorCode(e)] ?? toErrorMessage(e, "Could not save to KyDrive."));
            });
        }}
      >
        {state === "saving" ? "Saving to KyDrive…" : "Save to KyDrive"}
      </button>
      {error ? <span className="email-attachments-error" role="alert"> {error}</span> : null}
    </span>
  );
}
