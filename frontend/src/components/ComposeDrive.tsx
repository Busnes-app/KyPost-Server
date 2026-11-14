import type { Compose } from "../hooks/useCompose";
import { formatBytes, MAX_ATTACHMENT_BYTES } from "../app/compose";

const list = (items: string[]) => items.join(", ");

/** Compose's KyDrive notices: the over-limit offer, uploads in progress, and the outside-recipient check. */
export function ComposeDrive({ compose }: { compose: Compose }) {
  const { drive, sendComposeEmail, composeSending } = compose;
  const { driveOffer, driveUploads, driveOutside } = drive;
  return (
    <>
      {driveOffer.length > 0 ? (
        <div className="notice notice-warning compose-drive" role="alert">
          <p>
            {list(driveOffer.map((f) => f.name))} {driveOffer.length === 1 ? "is" : "are"} over the {formatBytes(MAX_ATTACHMENT_BYTES)} attachment limit.
          </p>
          <div className="compose-drive-actions">
            <button type="button" onClick={() => void drive.acceptDriveOffer()}>Upload to KyDrive and send a link</button>
            <button type="button" className="btn-ghost" onClick={() => drive.setDriveOffer([])}>Cancel</button>
          </div>
        </div>
      ) : null}
      {driveUploads.map((u) => (
        <div key={u.id} className="compose-drive-upload" role="status">
          <span className="compose-attachment-name">{u.name}</span>
          <progress max={u.size || 1} value={u.loaded} aria-label={`Uploading ${u.name} to KyDrive`} />
          <span className="compose-attachment-size">{formatBytes(u.loaded)} of {formatBytes(u.size)}</span>
          <button type="button" className="btn-ghost" onClick={() => drive.cancelDriveUpload(u.id)}>Cancel</button>
        </div>
      ))}
      {driveOutside ? (
        <div className="notice notice-warning compose-drive" role="alert">
          <p>
            {driveOutside.unverified
              ? `KyPost could not check whether ${list(driveOutside.addresses)} can open the KyDrive links in this message.`
              : `${list(driveOutside.addresses)} ${driveOutside.addresses.length === 1 ? "is" : "are"} not in your organisation and cannot open the KyDrive links in this message.`}
          </p>
          <div className="compose-drive-actions">
            <button
              type="button"
              disabled={composeSending}
              onClick={() => {
                const at = driveOutside.sendAt;
                drive.setDriveOutside(null);
                void sendComposeEmail(at, true);
              }}
            >
              Send anyway
            </button>
            {driveOutside.canAttach ? (
              <button type="button" disabled={composeSending} onClick={() => void drive.attachDriveFilesInstead()}>Attach the files instead</button>
            ) : null}
            <button type="button" className="btn-ghost" onClick={() => drive.setDriveOutside(null)}>Cancel</button>
          </div>
        </div>
      ) : null}
    </>
  );
}
