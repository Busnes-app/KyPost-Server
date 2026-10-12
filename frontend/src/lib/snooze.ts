// Snooze wake-time presets and the list's sort key for mail back from snooze.

/** The datetime-local form of now, for an input's min. */
export const localNow = () => new Date(Date.now() - new Date().getTimezoneOffset() * 60_000).toISOString().slice(0, 16);

const localDay = (from: Date, days: number, hour: number) =>
  new Date(from.getFullYear(), from.getMonth(), from.getDate() + days, hour);

export function snoozePresets(now: Date): Array<{ label: string; at: Date }> {
  const later = new Date(now.getTime() + 3 * 3_600_000);
  return [
    ...(later < localDay(now, 0, 21) ? [{ label: "Later today", at: later }] : []),
    { label: "Tomorrow morning", at: localDay(now, 1, 8) },
    // Days until the next Monday; on a Monday, a full week.
    { label: "Next week", at: localDay(now, (8 - now.getDay()) % 7 || 7, 8) }
  ];
}

/** A woken email sorts as if it arrived when it woke. */
export const sortTime = (email: { atUtc: string; wokenAt?: string }) => email.wokenAt ?? email.atUtc;
