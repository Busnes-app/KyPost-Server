import { useState, type FormEvent } from "react";
import type { Compose } from "../hooks/useCompose";
import { formatBytes, MAX_ATTACHMENT_BYTES } from "../app/compose";
import { guestDayChoices } from "../api/kydrive";
import { reauthenticate, reauthenticateWithSSO } from "../api/auth";
import { toErrorMessage } from "../api/client";
import { useAuth } from "../auth";

const list = (items: string[]) => items.join(", ");

/** Compose's KyDrive notices: the over-limit offer, uploads in progress, and the outside-recipient check. */
export function ComposeDrive({ compose }: { compose: Compose }) {
  const { drive, sendComposeEmail, composeSending } = compose;
  const { driveOffer, driveUploads, driveOutside, driveGuestStepUp, driveStatus } = drive;
  const maxDays = driveStatus.guestMaxDays ?? 0;
  const [guestDays, setGuestDays] = useState(7);
  const days = Math.min(guestDays, maxDays) || 1;
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
          {driveOutside.canGuest ? (
            <div className="compose-drive-guest">
              <p>A guest link lets anyone who has it view and download the file, without signing in, until it expires. You can revoke it in KyDrive.</p>
              <div className="compose-drive-actions">
                <label>
                  Guest links expire after{" "}
                  <select value={days} onChange={(e) => setGuestDays(Number(e.target.value))} disabled={composeSending}>
                    {guestDayChoices(maxDays).map((d) => <option key={d} value={d}>{d === 1 ? "1 day" : `${d} days`}</option>)}
                  </select>
                </label>
                <button
                  type="button"
                  disabled={composeSending}
                  onClick={async () => {
                    const at = driveOutside.sendAt;
                    if (await drive.sendWithGuestLinks(days, at)) void sendComposeEmail(at, true);
                  }}
                >
                  Send a guest link instead
                </button>
              </div>
            </div>
          ) : null}
        </div>
      ) : null}
      {driveGuestStepUp ? <GuestStepUp compose={compose} /> : null}
    </>
  );
}

/** Confirm it is you before guest links are made (KyPost's step-up, then the held send). */
function GuestStepUp({ compose }: { compose: Compose }) {
  const { drive, sendComposeEmail, composeSending } = compose;
  const sso = useAuth().ssoSession === true;
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function confirm(reauth: () => Promise<void>) {
    setBusy(true);
    setError("");
    try {
      const held = await drive.confirmGuestStepUp(reauth);
      if (held) void sendComposeEmail(held.sendAt, true);
    } catch (e) {
      setError(toErrorMessage(e, "could not confirm it is you"));
      setCode("");
    } finally {
      setPassword("");
      setBusy(false);
    }
  }
  const submit = (e: FormEvent) => {
    e.preventDefault();
    void confirm(() => reauthenticate(password, code.trim()));
  };
  return (
    <div className="notice notice-warning compose-drive" role="alert">
      <p>Guest links reach people outside your organisation, so confirm it is you before KyPost makes them.</p>
      {sso ? (
        <div className="compose-drive-actions">
          <button type="button" disabled={busy || composeSending} onClick={() => void confirm(reauthenticateWithSSO)}>Confirm with KySignOn</button>
          <button type="button" className="btn-ghost" onClick={() => drive.setDriveGuestStepUp(null)}>Cancel</button>
        </div>
      ) : (
        <form className="compose-drive-stepup" onSubmit={submit}>
          <label>Account password <input type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} required /></label>
          <label>Two-factor code, if you use one <input type="text" autoComplete="one-time-code" spellCheck={false} value={code} onChange={(e) => setCode(e.target.value)} /></label>
          <div className="compose-drive-actions">
            <button type="submit" disabled={busy || composeSending || !password}>{busy ? "Confirming…" : "Confirm and send"}</button>
            <button type="button" className="btn-ghost" onClick={() => drive.setDriveGuestStepUp(null)}>Cancel</button>
          </div>
        </form>
      )}
      {error ? <p className="notice notice-error">{error}</p> : null}
    </div>
  );
}
