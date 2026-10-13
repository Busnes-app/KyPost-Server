// Scheduled-send times. The server accepts one minute to 366 days ahead.
const MIN_LEAD_MS = 60_000;
const MAX_LEAD_MS = 366 * 24 * 60 * 60_000;

/** Whether the server will accept at as a send time, given now. */
export function validSendAt(at: Date, now: Date): boolean {
  const lead = at.getTime() - now.getTime();
  return Number.isFinite(lead) && lead >= MIN_LEAD_MS && lead <= MAX_LEAD_MS;
}

const at8 = (d: Date, addDays: number) => new Date(d.getFullYear(), d.getMonth(), d.getDate() + addDays, 8, 0, 0, 0);

/** In an hour, tomorrow 08:00 and next Monday 08:00, in local time. */
export function schedulePresets(now: Date): { label: string; at: Date }[] {
  const hour = new Date(now.getTime() + 60 * 60_000);
  hour.setSeconds(0, 0);
  const toMonday = ((8 - now.getDay()) % 7) || 7;
  return [
    { label: "In 1 hour", at: hour },
    { label: "Tomorrow morning", at: at8(now, 1) },
    { label: "Monday morning", at: at8(now, toMonday) }
  ];
}
