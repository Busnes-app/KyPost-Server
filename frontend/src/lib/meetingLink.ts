// "New meeting" deep link into KyCalendar (docs/SUITE-INTEGRATIONS.md, E).
// Every value is capped and encoded here; KyCalendar treats them as untrusted
// text too. Offered only when KyCalendar is in the user's suite app list.
import type { SuiteApp } from "../components/AppRail";

const MAX_TITLE_BYTES = 255;
const MAX_BODY_BYTES = 2000;
const MAX_ATTENDEES = 100;
// Bare address, no comma (the list separator), whitespace or controls.
const addressRe = /^[^\s@<>",;\p{Cc}\p{Cf}]{1,64}@[^\s@<>",;\p{Cc}\p{Cf}]{1,190}$/u;
const controlRe = /[\p{Cc}\p{Cf}]/gu;

/** The origin of KyCalendar's suite `url`; "" when it is absent or not https. */
export function calendarOrigin(apps: SuiteApp[]): string {
  const url = apps.find((a) => a.id === "kycalendar")?.url ?? "";
  try {
    const u = new URL(url);
    return u.protocol === "https:" ? u.origin : "";
  } catch {
    return "";
  }
}

/** The longest prefix of value within limit UTF-8 bytes, never splitting a character. */
function cut(value: string, limit: number): string {
  const encoder = new TextEncoder();
  let out = "";
  let bytes = 0;
  for (const ch of value) {
    bytes += encoder.encode(ch).length;
    if (bytes > limit) break;
    out += ch;
  }
  return out;
}

export type Meeting = { title?: string; attendees?: string[]; body?: string };

export function newMeetingUrl(origin: string, m: Meeting): string {
  const q = new URLSearchParams();
  const title = cut((m.title ?? "").replace(controlRe, " ").replace(/\s+/g, " ").trim(), MAX_TITLE_BYTES);
  if (title) q.set("title", title);
  const seen = new Set<string>();
  const attendees: string[] = [];
  for (const raw of m.attendees ?? []) {
    const a = raw.trim();
    if (attendees.length >= MAX_ATTENDEES || !addressRe.test(a) || seen.has(a.toLowerCase())) continue;
    seen.add(a.toLowerCase());
    attendees.push(a);
  }
  if (attendees.length) q.set("attendees", attendees.join(","));
  // Newlines and tabs stay in a body; other controls go.
  const body = cut((m.body ?? "").replace(/(?![\n\t])[\p{Cc}\p{Cf}]/gu, ""), MAX_BODY_BYTES).trim();
  if (body) q.set("body", body);
  const query = q.toString();
  return `${origin}/new${query ? `?${query}` : ""}`;
}

/** Opens the editor in a new tab without giving it this window. */
export function openNewMeeting(origin: string, m: Meeting): void {
  window.open(newMeetingUrl(origin, m), "_blank", "noopener,noreferrer");
}
