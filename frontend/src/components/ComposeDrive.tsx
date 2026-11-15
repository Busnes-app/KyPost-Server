import { useEffect, useRef } from "react";
import type { Compose } from "../hooks/useCompose";
import type { PickMode } from "../api/kydrive";
import { initMenuButton } from "../ky-ui/menu";
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
          {u.loaded === undefined ? (
            <>
              <progress aria-label={`Copying ${u.name} from KyDrive`} />
              <span className="compose-attachment-size">Copying from KyDrive</span>
            </>
          ) : (
            <>
              <progress max={u.size || 1} value={u.loaded} aria-label={`Uploading ${u.name} to KyDrive`} />
              <span className="compose-attachment-size">{formatBytes(u.loaded)} of {formatBytes(u.size)}</span>
            </>
          )}
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

/**
 * "Attach from KyDrive" (contract I): ky-ui's menu button with "Share a link"
 * and "Attach a copy". The choice's click opens KyDrive's picker popup.
 */
export function DriveAttachMenu({ onPick, disabled }: { onPick: (mode: PickMode) => void; disabled: boolean }) {
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => initMenuButton(button.current!), []);
  return (
    <div className="ky-menu compose-drive-menu">
      <button ref={button} type="button" className="compose-attach" aria-haspopup="menu" aria-expanded="false" aria-controls="compose-drive-menu" disabled={disabled}>
        Attach from KyDrive
      </button>
      <div className="ky-menu-list" id="compose-drive-menu" role="menu" aria-label="Attach from KyDrive" hidden>
        <button type="button" role="menuitem" tabIndex={-1} onClick={() => onPick("link")}>Share a link</button>
        <button type="button" role="menuitem" tabIndex={-1} onClick={() => onPick("copy")}>Attach a copy</button>
      </div>
    </div>
  );
}
