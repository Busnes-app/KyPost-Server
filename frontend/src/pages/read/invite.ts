// Calendar invitation card: the parser, the part to read and the RFC 5546
// REPLY. Rules follow kypost-Linux core/domain/CalendarInvite.cpp (a port of
// Android's CalendarInvite.kt), minus Windows zone names. Everything here reads
// sender-controlled bytes; the card renders the results as text only.
import { HttpError } from "../../api/client";
import type { AttachmentInfo } from "./types";

export const MAX_INVITE_BYTES = 256 * 1024;
const MAX_EVENTS = 50;

export type Partstat = "ACCEPTED" | "TENTATIVE" | "DECLINED";

export type InviteEvent = {
  uid: string;
  sequence: number;
  summary: string;
  location: string;
  /** Bare address from mailto:, never the sender-chosen CN; "" when unusable. */
  organizer: string;
  attendees: string[];
  begin: Date;
  /** Exclusive for all-day events. */
  end: Date;
  allDay: boolean;
  repeats: boolean;
  /** A TZID that resolved to nothing, so local time was used; "" otherwise. */
  unknownZone: string;
  canReply: boolean;
};

export type Invite = { method: string; event: InviteEvent };

type Line = { name: string; params: Map<string, string>; value: string };

const nameRe = /^[A-Z0-9-]{1,64}$/;
const methodRe = /^[A-Z][A-Z0-9-]{0,31}$/;
const dateRe = /^(\d{4})(\d{2})(\d{2})$/;
const dateTimeRe = /^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})(Z?)$/;
const durationRe = /^([+-]?)P(?:(\d{1,6})W|(?:(\d{1,6})D)?(?:T(?:(\d{1,6})H)?(?:(\d{1,6})M)?(?:(\d{1,6})S)?)?)$/;
const addressRe = /^[^\s@<>"\p{Cc}\p{Cf}]{1,64}@[^\s@<>"\p{Cc}\p{Cf}]{1,190}$/u;
const ianaRe = /^[A-Za-z][A-Za-z0-9_+-]*(?:\/[A-Za-z0-9_+-]+)*$/;
// Controls other than newline and tab, and format characters (bidi overrides)
// other than ZWJ: they could reorder or hide text on the card.
const invisibleRe = /(?![\n\t‍])[\p{Cc}\p{Cf}]/gu;
const isoControlRe = /[\u0000-\u001f\u007f-\u009f]/;

/** A listed part's bare media type; the listing is server data, so tolerate a missing field. */
export const partType = (a: AttachmentInfo) => String(a.mimeType ?? "").split(";")[0].trim().toLowerCase();

/** The listed part to read: text/calendar first, else application/ics or a .ics name. */
export function inviteAttachment(listed: AttachmentInfo[]): AttachmentInfo | undefined {
  const found = listed.find((a) => partType(a) === "text/calendar")
    ?? listed.find((a) => partType(a) === "application/ics" || /\.ics$/i.test(String(a.name ?? "").trim()));
  return found && found.size >= 1 && found.size <= MAX_INVITE_BYTES ? found : undefined;
}

/** RFC 5545 §3.1 unfolding on bytes, so a fold inside a UTF-8 sequence rejoins it. */
function unfold(bytes: Uint8Array): Uint8Array {
  const out = new Uint8Array(bytes.length);
  let n = 0;
  for (let i = 0; i < bytes.length; i++) {
    const lf = bytes[i] === 0x0a ? i : bytes[i] === 0x0d && bytes[i + 1] === 0x0a ? i + 1 : -1;
    if (lf >= 0 && (bytes[lf + 1] === 0x20 || bytes[lf + 1] === 0x09)) {
      i = lf + 1;
      continue;
    }
    out[n++] = bytes[i];
  }
  return out.subarray(0, n);
}

/** `NAME *(;PARAM=value) : value`; quoted parameter values may hold `:` or `;`. */
function contentLine(line: string): Line | null {
  let i = 0;
  while (i < line.length && line[i] !== ";" && line[i] !== ":") i++;
  if (i === 0 || i === line.length) return null;
  const name = line.slice(0, i).toUpperCase();
  if (!nameRe.test(name)) return null;
  const params = new Map<string, string>();
  while (line[i] === ";") {
    const eq = line.indexOf("=", i);
    if (eq < 0) return null;
    const param = line.slice(i + 1, eq).toUpperCase();
    if (!nameRe.test(param)) return null;
    i = eq + 1;
    let value = "";
    while (i < line.length && line[i] !== ";" && line[i] !== ":") {
      if (line[i] === '"') {
        const close = line.indexOf('"', i + 1);
        if (close < 0) return null;
        value += line.slice(i + 1, close);
        i = close + 1;
      } else {
        value += line[i++];
      }
    }
    if (i === line.length) return null;
    if (!params.has(param)) params.set(param, value);
  }
  return { name, params, value: line.slice(i + 1) };
}

function visible(value: string, limit: number): string {
  return Array.from(value.replace(invisibleRe, "")).slice(0, limit).join("").trim();
}

/** RFC 5545 §3.3.11 unescape, then visible(). */
function text(line: Line | undefined, limit = 1000): string {
  return line ? visible(line.value.replace(/\\(.)/g, (_, c: string) => (c === "n" || c === "N" ? "\n" : c)), limit) : "";
}

function mailto(value: string): string {
  const address = value.trim().replace(/^mailto:/i, "");
  return addressRe.test(address) ? address : "";
}

export function isRsvpAddress(address: string): boolean {
  return addressRe.test(address);
}

/** Milliseconds `zone` is ahead of UTC at instant t; throws for an unknown zone. */
function offsetAt(t: number, zone: string): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone: zone, hourCycle: "h23", year: "numeric", month: "numeric", day: "numeric", hour: "numeric", minute: "numeric", second: "numeric"
  }).formatToParts(new Date(t));
  const get = (type: string) => Number(parts.find((p) => p.type === type)?.value);
  return Date.UTC(get("year"), get("month") - 1, get("day"), get("hour"), get("minute"), get("second")) - Math.floor(t / 1000) * 1000;
}

/** A wall time in an IANA zone as an instant; a spring-forward gap moves it forward. */
function zoned(wall: number, zone: string): Date {
  const first = wall - offsetAt(wall, zone);
  return new Date(wall - offsetAt(first, zone));
}

/** The IANA id as given, then its last three or two segments (Mozilla's `/mozilla.org/…/America/New_York`). */
function resolveZone(tzid: string): string | null {
  if (tzid.length > 128) return null;
  const segments = tzid.split("/").filter(Boolean);
  for (const candidate of [tzid, segments.slice(-3).join("/"), segments.slice(-2).join("/")]) {
    if (!ianaRe.test(candidate)) continue;
    try {
      new Intl.DateTimeFormat("en-US", { timeZone: candidate });
      return candidate;
    } catch {
      // not a zone this browser knows
    }
  }
  return null;
}

type EventTime = { at: Date; allDay: boolean; unknownZone: string };

function validDay(y: number, m: number, d: number): boolean {
  const t = new Date(Date.UTC(y, m - 1, d));
  return t.getUTCFullYear() === y && t.getUTCMonth() === m - 1 && t.getUTCDate() === d;
}

function eventTime(line: Line): EventTime | null {
  const value = line.value.trim();
  const day = dateRe.exec(value);
  if (line.params.get("VALUE")?.toUpperCase() === "DATE" || day) {
    if (!day) return null;
    const [y, m, d] = day.slice(1).map(Number);
    return validDay(y, m, d) ? { at: new Date(y, m - 1, d), allDay: true, unknownZone: "" } : null;
  }
  const dt = dateTimeRe.exec(value);
  if (!dt) return null;
  const [y, mo, d, h, mi, s] = dt.slice(1, 7).map(Number);
  if (!validDay(y, mo, d) || h > 23 || mi > 59 || s > 59) return null;
  if (dt[7]) return { at: new Date(Date.UTC(y, mo - 1, d, h, mi, s)), allDay: false, unknownZone: "" };
  const tzid = line.params.get("TZID")?.trim() ?? "";
  const zone = tzid ? resolveZone(tzid) : null;
  if (zone) return { at: zoned(Date.UTC(y, mo - 1, d, h, mi, s), zone), allDay: false, unknownZone: "" };
  return { at: new Date(y, mo - 1, d, h, mi, s), allDay: false, unknownZone: tzid ? visible(tzid, 64) : "" };
}

/** RFC 5545 §3.3.6: days and weeks are nominal, hours, minutes and seconds exact. */
function addDuration(start: Date, value: string): Date | null {
  const m = durationRe.exec(value);
  if (!m || m.slice(2).every((g) => g === undefined)) return null;
  const n = (i: number) => Number(m[i] ?? 0);
  const sign = m[1] === "-" ? -1 : 1;
  const end = new Date(start);
  end.setDate(end.getDate() + sign * (7 * n(2) + n(3)));
  return new Date(end.getTime() + sign * 1000 * (3600 * n(4) + 60 * n(5) + n(6)));
}

function toEvent(props: Map<string, Line>, attendees: string[]): { event: InviteEvent; isOverride: boolean } | null {
  const dtstart = props.get("DTSTART");
  const start = dtstart && eventTime(dtstart);
  if (!start) return null;
  let endTime: EventTime | null = null;
  const dtend = props.get("DTEND");
  if (dtend && !(endTime = eventTime(dtend))) return null;
  const duration = props.get("DURATION");
  const end = endTime?.at ?? (duration && addDuration(start.at, duration.value.trim()))
    ?? (start.allDay ? new Date(start.at.getFullYear(), start.at.getMonth(), start.at.getDate() + 1) : start.at);
  // Untrimmed: the organizer matches the REPLY on the exact UID text.
  const rawUid = props.get("UID")?.value ?? "";
  const uid = rawUid.trim() && rawUid.length <= 1000 && !isoControlRe.test(rawUid) ? rawUid : "";
  const organizer = mailto(props.get("ORGANIZER")?.value ?? "");
  // A SEQUENCE that cannot be read back exactly would answer the wrong revision.
  const rawSequence = props.get("SEQUENCE")?.value.trim();
  const sequence = rawSequence === undefined ? 0 : /^\+?\d{1,9}$/.test(rawSequence) ? Number(rawSequence) : -1;
  return {
    isOverride: props.has("RECURRENCE-ID"),
    event: {
      uid,
      sequence: Math.max(sequence, 0),
      summary: text(props.get("SUMMARY")),
      location: text(props.get("LOCATION")),
      organizer,
      attendees,
      begin: start.at,
      end: end.getTime() > start.at.getTime() ? end : start.at,
      allDay: start.allDay,
      repeats: props.has("RRULE"),
      unknownZone: start.unknownZone || endTime?.unknownZone || "",
      canReply: uid !== "" && organizer !== "" && sequence >= 0
    }
  };
}

/** The subset of RFC 5545 the card needs, or null for anything it cannot vouch for. */
export function parseInvite(bytes: Uint8Array): Invite | null {
  if (bytes.length === 0 || bytes.length > MAX_INVITE_BYTES) return null;
  const lines = new TextDecoder().decode(unfold(bytes)).replace(/^﻿/, "").split(/\r\n|\r|\n/).filter((l) => l.trim() !== "");
  const stack: string[] = [];
  let method = "";
  let props: Map<string, Line> | null = null;
  let attendees: string[] = [];
  const events: { event: InviteEvent; isOverride: boolean }[] = [];
  for (const [index, raw] of lines.entries()) {
    const line = contentLine(raw);
    if (!line) return null;
    if (index === 0 && !(line.name === "BEGIN" && line.value.trim().toUpperCase() === "VCALENDAR")) return null;
    if (index > 0 && stack.length === 0) return null;
    const component = line.value.trim().toUpperCase();
    if (line.name === "BEGIN") {
      if (component === "VCALENDAR" && stack.length) return null;
      if (component === "VEVENT") {
        if (stack.length !== 1 || events.length >= MAX_EVENTS) return null;
        props = new Map();
        attendees = [];
      }
      stack.push(component);
    } else if (line.name === "END") {
      if (stack.pop() !== component) return null;
      if (component === "VEVENT") {
        const parsed = props && toEvent(props, attendees);
        if (!parsed) return null;
        events.push(parsed);
        props = null;
      }
    } else if (stack.length === 1 && line.name === "METHOD") {
      method = methodRe.test(component) ? component : "";
    } else if (stack.length === 2 && stack[1] === "VEVENT" && props) {
      // The VEVENT's own properties only: a VALARM inside it has a DESCRIPTION too.
      if (line.name === "ATTENDEE") {
        const address = mailto(line.value);
        if (address) attendees.push(address);
      } else if (!props.has(line.name)) {
        props.set(line.name, line);
      }
    }
  }
  // An override is never the headline event, so no series master means no card.
  const master = events.find((e) => !e.isOverride);
  return stack.length === 0 && master ? { method, event: master.event } : null;
}

/** The user's address the invite names, else the first one known. */
export function rsvpAttendee(event: InviteEvent, own: string[]): string {
  const usable = own.map((a) => a.trim()).filter(isRsvpAddress);
  return usable.find((a) => event.attendees.some((b) => b.toLowerCase() === a.toLowerCase())) ?? usable[0] ?? "";
}

/** RFC 5545 §3.1: at most 75 octets a line, never splitting a UTF-8 sequence. */
function fold(line: string): string {
  let out = "";
  let octets = 0;
  for (const ch of line) {
    const size = new TextEncoder().encode(ch).length;
    if (octets + size > 75) {
      out += "\r\n ";
      octets = 1;
    }
    out += ch;
    octets += size;
  }
  return out;
}

const verbs: Record<Partstat, [string, string]> = {
  ACCEPTED: ["Accepted", "accepted"],
  TENTATIVE: ["Tentative", "tentatively accepted"],
  DECLINED: ["Declined", "declined"]
};

/** What POST /api/mail/send carries for one RSVP; null unless the event can be answered as attendee. */
export function rsvpMail(event: InviteEvent, attendee: string, partstat: Partstat, now: Date) {
  if (!event.canReply || !isRsvpAddress(attendee)) return null;
  // Always the series master, as the native clients: no RECURRENCE-ID.
  const ics = [
    "BEGIN:VCALENDAR",
    "VERSION:2.0",
    "PRODID:-//KyPost//Web//EN",
    "METHOD:REPLY",
    "BEGIN:VEVENT",
    `UID:${event.uid}`,
    `SEQUENCE:${event.sequence}`,
    `DTSTAMP:${now.toISOString().replace(/[-:]/g, "").replace(/\.\d{3}/, "")}`,
    `ORGANIZER:mailto:${event.organizer}`,
    `ATTENDEE;PARTSTAT=${partstat}:mailto:${attendee}`,
    "END:VEVENT",
    "END:VCALENDAR"
  ].map((l) => fold(l) + "\r\n").join("");
  const title = event.summary.replace(/\s+/g, " ").trim();
  return {
    to: event.organizer,
    subject: `${verbs[partstat][0]}: ${title || "Invitation"}`,
    body: `${attendee} has ${verbs[partstat][1]} this invitation.`,
    ics
  };
}

type Post = <T>(path: string, body: unknown) => Promise<T>;

export type CalendarAnswer =
  | { kind: "answered" }
  | { kind: "off" }
  | { kind: "conflict"; message: string }
  | { kind: "idle"; message: string };

const codeOf = (e: unknown) => (e instanceof HttpError ? (e.body as { code?: unknown } | undefined)?.code : undefined);
const errorOf = (e: unknown, fallback: string) => {
  const msg = e instanceof HttpError ? (e.body as { error?: unknown } | undefined)?.error : undefined;
  return typeof msg === "string" && msg ? msg : fallback;
};

/** POST /api/calendar/rsvp, mapped per docs/KYCALENDAR.md "Respond". */
export async function answerThroughKyCalendar(
  post: Post,
  req: { messageId: string; mailbox: string; index: number; partstat: Partstat }
): Promise<CalendarAnswer> {
  try {
    const res = await post<{ outcome?: string }>("/api/calendar/rsvp", req);
    return res.outcome === "answered" ? { kind: "answered" } : { kind: "idle", message: "KyCalendar did not confirm the answer; try again." };
  } catch (e) {
    const code = codeOf(e);
    if (code === "kycalendar_off") return { kind: "off" };
    if (code === "conflict") return { kind: "conflict", message: "The event changed since this invitation. Reload it and answer again." };
    return { kind: "idle", message: errorOf(e, "KyCalendar could not be reached; try again.") };
  }
}

export type MailAnswer =
  | { kind: "sent" }
  /** Delivered, but as plain mail the organizer's calendar will not read. */
  | { kind: "plain"; message: string }
  | { kind: "retry"; message: string }
  /** May have gone out: do not offer a second send. */
  | { kind: "uncertain"; message: string };

/** POST /api/mail/send with calendarReply; only `"calendarReply": true` counts as sent. */
export async function answerByMail(post: Post, mail: NonNullable<ReturnType<typeof rsvpMail>>, from: string): Promise<MailAnswer> {
  try {
    const res = await post<{ calendarReply?: boolean }>("/api/mail/send", {
      to: mail.to,
      subject: mail.subject,
      body: mail.body,
      mode: "plain",
      from,
      encrypt: false,
      sign: false,
      calendarReply: { ics: mail.ics }
    });
    return res.calendarReply === true
      ? { kind: "sent" }
      : { kind: "plain", message: "The organizer got your answer as ordinary mail; their calendar may not update." };
  } catch (e) {
    if (e instanceof HttpError && (e.status < 500 || e.status === 503)) {
      return { kind: "retry", message: e.message };
    }
    return { kind: "uncertain", message: "Your answer may have been sent. Check Sent before answering again." };
  }
}
