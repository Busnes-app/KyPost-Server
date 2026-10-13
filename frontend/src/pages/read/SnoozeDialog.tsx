import { useRef, useState } from "react";
import { useDialogOpen } from "../../hooks/useDialogOpen";
import { localDateTimeValue } from "../../lib/localDateTime";
import { snoozePresets } from "../../lib/snooze";

type Props = {
  open: boolean;
  busy: boolean;
  /** wakeAt is an ISO (UTC) time; the caller posts the action. */
  onSnooze: (wakeAt: string) => void;
  onCancel: () => void;
};

export function SnoozeDialog({ open, busy, onSnooze, onCancel }: Props) {
  const dialogRef = useRef<HTMLDialogElement | null>(null);
  const [custom, setCustom] = useState("");
  useDialogOpen(dialogRef, open);

  const finish = (wakeAt?: Date) => {
    setCustom("");
    if (wakeAt) onSnooze(wakeAt.toISOString());
    else onCancel();
  };

  return (
    <dialog
      ref={dialogRef}
      className="rules-help-backdrop"
      aria-label="Snooze"
      onCancel={(event) => {
        event.preventDefault();
        finish();
      }}
      onClick={(event) => {
        if (event.target === dialogRef.current) finish();
      }}
    >
      {open ? (
        <div className="rules-help-window" style={{ width: "min(420px, 94vw)" }} onClick={(event) => event.stopPropagation()}>
          <div className="rules-help-head">
            <h3>Snooze until</h3>
          </div>
          <div className="rules-help-body">
            <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
              {snoozePresets(new Date()).map((preset) => (
                <button key={preset.label} type="button" disabled={busy} onClick={() => finish(preset.at)}>
                  {preset.label}
                </button>
              ))}
            </div>
            <label style={{ display: "block", marginTop: 16 }}>
              Pick a date and time
              <input type="datetime-local" min={localDateTimeValue(new Date())} value={custom} onChange={(event) => setCustom(event.target.value)} />
            </label>
            <div style={{ display: "flex", gap: 8, marginTop: 16, justifyContent: "flex-end" }}>
              <button type="button" className="contacts-action" onClick={() => finish()}>
                Cancel
              </button>
              <button type="button" disabled={!custom || busy} onClick={() => finish(new Date(custom))}>
                Snooze
              </button>
            </div>
          </div>
        </div>
      ) : null}
    </dialog>
  );
}
