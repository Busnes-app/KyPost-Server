import { useRef } from "react";
import { useDialogOpen } from "../../hooks/useDialogOpen";
import { flagDueChoices } from "../../lib/flag";

type Props = {
  open: boolean;
  busy: boolean;
  /** KyCalendar follows this user's flags as tasks (GET /api/mail/flags `tasks`). */
  tasks: boolean;
  /** due is a local YYYY-MM-DD date, or "" for no date; the caller posts the action. */
  onFlag: (due: string) => void;
  onCancel: () => void;
};

// FlagDialog asks when a flagged message is due: a sheet of choices, full screen on phones.
export function FlagDialog({ open, busy, tasks, onFlag, onCancel }: Props) {
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
        <div className="rules-help-window due-sheet" onClick={(event) => event.stopPropagation()}>
          <div className="rules-help-head">
            <h3>When is it due?</h3>
          </div>
          <div className="rules-help-body">
            {tasks ? <p className="sec-muted">Also adds a task in KyCalendar.</p> : null}
            <div className="due-choices">
              {flagDueChoices(new Date()).map((choice) => (
                <button key={choice.label} type="button" disabled={busy} onClick={() => onFlag(choice.due)}>
                  {choice.date ? `${choice.label} · ${choice.date}` : choice.label}
                </button>
              ))}
            </div>
          </div>
          <div className="due-sheet-foot">
            <button type="button" className="contacts-action" onClick={onCancel}>
              Cancel
            </button>
          </div>
        </div>
      ) : null}
    </dialog>
  );
}
