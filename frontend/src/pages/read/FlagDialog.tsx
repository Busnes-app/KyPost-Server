import { useRef } from "react";
import { useDialogOpen } from "../../hooks/useDialogOpen";
import { flagDueChoices } from "../../lib/flag";

type Props = {
  open: boolean;
  busy: boolean;
  /** due is a local YYYY-MM-DD date, or "" for no date; the caller posts the action. */
  onFlag: (due: string) => void;
  onCancel: () => void;
};

// FlagDialog asks when a flagged message is due. With KyCalendar paired the flag makes a task.
export function FlagDialog({ open, busy, onFlag, onCancel }: Props) {
  const dialogRef = useRef<HTMLDialogElement | null>(null);
  useDialogOpen(dialogRef, open);
  return (
    <dialog
      ref={dialogRef}
      className="rules-help-backdrop"
      aria-label="Flag"
      onCancel={(event) => {
        event.preventDefault();
        onCancel();
      }}
      onClick={(event) => {
        if (event.target === dialogRef.current) onCancel();
      }}
    >
      {open ? (
        <div className="rules-help-window" style={{ width: "min(420px, 94vw)" }} onClick={(event) => event.stopPropagation()}>
          <div className="rules-help-head">
            <h3>When is it due?</h3>
          </div>
          <div className="rules-help-body">
            <p className="sec-muted">If KyCalendar is connected, the message also becomes a task there.</p>
            <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
              {flagDueChoices(new Date()).map((choice) => (
                <button key={choice.label} type="button" disabled={busy} onClick={() => onFlag(choice.due)}>
                  {choice.label}
                </button>
              ))}
            </div>
            <div style={{ display: "flex", gap: 8, marginTop: 16, justifyContent: "flex-end" }}>
              <button type="button" className="contacts-action" onClick={onCancel}>
                Cancel
              </button>
            </div>
          </div>
        </div>
      ) : null}
    </dialog>
  );
}
