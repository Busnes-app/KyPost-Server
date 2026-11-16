// Due choices for a flagged message's KyCalendar task (docs/KYCALENDAR.md, Tasks), as local dates.

const ymd = (d: Date) =>
  `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
const plus = (from: Date, days: number) => new Date(from.getFullYear(), from.getMonth(), from.getDate() + days);

// "Fri 20": the week choices show the day they resolve to.
const short = (d: Date) => `${d.toLocaleDateString(undefined, { weekday: "short" })} ${d.getDate()}`;

/** Today, Tomorrow, This week (Friday; Monday to Thursday only), Next week (Monday), No date (""). */
export function flagDueChoices(now: Date): Array<{ label: string; due: string; date?: string }> {
  const day = now.getDay();
  const friday = plus(now, 5 - day);
  const monday = plus(now, (8 - day) % 7 || 7);
  return [
    { label: "Today", due: ymd(now) },
    { label: "Tomorrow", due: ymd(plus(now, 1)) },
    ...(day >= 1 && day <= 4 ? [{ label: "This week", due: ymd(friday), date: short(friday) }] : []),
    { label: "Next week", due: ymd(monday), date: short(monday) },
    { label: "No date", due: "" }
  ];
}
