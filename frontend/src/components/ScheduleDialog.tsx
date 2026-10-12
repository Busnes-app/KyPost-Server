import { useRef, useState } from "react";
import { useDialogOpen } from "../hooks/useDialogOpen";
import { localDateTimeValue, schedulePresets, validSendAt } from "../lib/schedule";

const formatTime = (d: Date) => d.toLocaleString(undefined, { weekday: "short", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });

/** Picks a send time: a preset or a datetime-local value at least a minute ahead. */
export function ScheduleDialog({ open, onSchedule, onCancel }: { open: boolean; onSchedule: (at: Date) => void; onCancel: () => void }) {
  const ref = useRef<HTMLDialogElement | null>(null);
  useDialogOpen(ref, open);
  const [custom, setCustom] = useState("");
  const now = new Date();
  const customAt = custom ? new Date(custom) : null;
  const customOk = customAt !== null && validSendAt(customAt, now);
  return (
    <dialog
      ref={ref}
      aria-labelledby="schedule-dialog-title"
      onCancel={(event) => {
        event.preventDefault();
        onCancel();
      }}
    >
      <h2 id="schedule-dialog-title">Schedule send</h2>
      <p>The message is frozen now and sent at the chosen time. If your account or keys change before then, it returns to Drafts unsent.</p>
      <div style={{ display: "grid", gap: 8 }}>
        {schedulePresets(now).map((preset) => (
          <button key={preset.label} type="button" className="button" onClick={() => onSchedule(preset.at)}>
            {preset.label} ({formatTime(preset.at)})
          </button>
        ))}
        <label>
          Pick a date and time
          <input
            type="datetime-local"
            min={localDateTimeValue(new Date(now.getTime() + 60_000))}
            value={custom}
            onChange={(event) => setCustom(event.target.value)}
          />
        </label>
        {custom && !customOk ? <p className="notice notice-error">Choose a time between one minute and a year from now.</p> : null}
        <div style={{ display: "flex", gap: 8 }}>
          <button type="button" className="button" disabled={!customOk} onClick={() => customAt && onSchedule(customAt)}>Schedule</button>
          <button type="button" className="button" onClick={onCancel}>Cancel</button>
        </div>
      </div>
    </dialog>
  );
}
