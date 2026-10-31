import { describe, expect, it, vi } from "vitest";
import { HttpError } from "../../api/client";
import { MAX_INVITE_BYTES, answerByMail, answerThroughKyCalendar, inviteAttachment, parseInvite, rsvpAttendee, rsvpMail } from "./invite";

const enc = (s: string) => new TextEncoder().encode(s);
const ics = (event: string, method = "REQUEST") =>
  enc(`BEGIN:VCALENDAR\r\nVERSION:2.0\r\nMETHOD:${method}\r\nBEGIN:VEVENT\r\n${event}\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n`);
const base = [
  "UID:abc-123@example.com",
  "SEQUENCE:2",
  "SUMMARY:Planning\\, Q4",
  "LOCATION:Room 1",
  "DTSTART:20261020T100000Z",
  "DTEND:20261020T110000Z",
  "ORGANIZER;CN=\"Boss: Big\":mailto:alice@example.com",
  "ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:Bob@Example.net"
].join("\r\n");

describe("inviteAttachment", () => {
  const at = (index: number, mimeType: string, name = "x", size = 10) => ({ index, mimeType, name, size });
  it("prefers text/calendar over a .ics file and refuses empty or oversized parts", () => {
    expect(inviteAttachment([at(0, "application/ics", "invite.ics"), at(1, "text/calendar; method=REQUEST")])?.index).toBe(1);
    expect(inviteAttachment([at(0, "application/octet-stream", "Invite.ICS")])?.index).toBe(0);
    expect(inviteAttachment([at(0, "text/calendar", "x", 0)])).toBeUndefined();
    expect(inviteAttachment([at(0, "text/calendar", "x", MAX_INVITE_BYTES + 1)])).toBeUndefined();
    expect(inviteAttachment([at(0, "application/pdf")])).toBeUndefined();
  });
});

describe("parseInvite", () => {
  it("reads the card fields and keeps the CN out of the organizer", () => {
    const invite = parseInvite(ics(base));
    expect(invite?.method).toBe("REQUEST");
    const e = invite!.event;
    expect(e).toMatchObject({ uid: "abc-123@example.com", sequence: 2, summary: "Planning, Q4", location: "Room 1", organizer: "alice@example.com", canReply: true, allDay: false });
    expect(e.begin.toISOString()).toBe("2026-10-20T10:00:00.000Z");
    expect(e.end.toISOString()).toBe("2026-10-20T11:00:00.000Z");
    expect(e.attendees).toEqual(["Bob@Example.net"]);
  });

  it("unfolds on bytes, so a fold inside a UTF-8 sequence rejoins it", () => {
    const summary = enc("SUMMARY:Café");
    // Split the two bytes of é with CRLF + space.
    const folded = new Uint8Array([...summary.slice(0, -1), 0x0d, 0x0a, 0x20, ...summary.slice(-1)]);
    const head = enc("BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:u\r\nDTSTART:20261020T100000Z\r\n");
    const tail = enc("\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n");
    expect(parseInvite(new Uint8Array([...head, ...folded, ...tail]))?.event.summary).toBe("Café");
  });

  it("strips bidi overrides and controls from text", () => {
    const e = parseInvite(ics(base.replace("SUMMARY:Planning\\, Q4", "SUMMARY:<b>evil</b>‮gnp.exe\u0007")))!.event;
    expect(e.summary).toBe("<b>evil</b>gnp.exe");
  });

  it("resolves TZIDs, including Mozilla-prefixed ones, and flags unknown zones", () => {
    const zoned = (tzid: string) => parseInvite(ics(base.replace("DTSTART:20261020T100000Z", `DTSTART;TZID=${tzid}:20260115T090000`).replace(/DTEND.*\r\n/, "")))!.event;
    expect(zoned("America/New_York").begin.toISOString()).toBe("2026-01-15T14:00:00.000Z");
    expect(zoned("/mozilla.org/20050126_1/America/New_York").begin.toISOString()).toBe("2026-01-15T14:00:00.000Z");
    expect(zoned("Europe/Berlin").unknownZone).toBe("");
    expect(zoned("Pacific Standard Time").unknownZone).toBe("Pacific Standard Time");
    expect(zoned("../../etc/passwd").unknownZone).toBe("../../etc/passwd");
  });

  it("reads all-day events with an exclusive end and DURATION", () => {
    const allDay = parseInvite(ics(base.replace("DTSTART:20261020T100000Z", "DTSTART;VALUE=DATE:20261020").replace(/DTEND.*\r\n/, "")))!.event;
    expect(allDay.allDay).toBe(true);
    expect(allDay.end.getDate()).toBe(21);
    const timed = parseInvite(ics(base.replace(/DTEND.*\r\n/, "DURATION:PT1H30M\r\n")))!.event;
    expect(timed.end.toISOString()).toBe("2026-10-20T11:30:00.000Z");
  });

  it("refuses what it cannot vouch for", () => {
    expect(parseInvite(enc(""))).toBeNull();
    expect(parseInvite(new Uint8Array(MAX_INVITE_BYTES + 1))).toBeNull();
    expect(parseInvite(enc("SUMMARY:x\r\nBEGIN:VCALENDAR\r\nEND:VCALENDAR"))).toBeNull();
    expect(parseInvite(ics(base.replace("DTSTART:20261020T100000Z", "DTSTART:20261340T100000Z")))).toBeNull();
    expect(parseInvite(ics(base).subarray(0, 120))).toBeNull();
    // Only an override: no series master to headline.
    expect(parseInvite(ics(`${base}\r\nRECURRENCE-ID:20261020T100000Z`))).toBeNull();
    const many = Array.from({ length: 51 }, () => `BEGIN:VEVENT\r\nUID:u\r\nDTSTART:20261020T100000Z\r\nEND:VEVENT`).join("\r\n");
    expect(parseInvite(enc(`BEGIN:VCALENDAR\r\n${many}\r\nEND:VCALENDAR\r\n`))).toBeNull();
  });

  it("puts the series master first and ignores a VALARM's properties", () => {
    const text = `BEGIN:VCALENDAR\r\nMETHOD:REQUEST\r\nBEGIN:VEVENT\r\nUID:u\r\nRECURRENCE-ID:20261021T100000Z\r\nSUMMARY:Override\r\nDTSTART:20261021T120000Z\r\nEND:VEVENT\r\nBEGIN:VEVENT\r\nUID:u\r\nBEGIN:VALARM\r\nSUMMARY:Alarm\r\nEND:VALARM\r\nSUMMARY:Master\r\nRRULE:FREQ=DAILY\r\nDTSTART:20261020T100000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n`;
    expect(parseInvite(enc(text))?.event).toMatchObject({ summary: "Master", repeats: true });
  });

  it("offers no reply for an unreadable SEQUENCE, a missing UID or a non-mailto organizer", () => {
    expect(parseInvite(ics(base.replace("SEQUENCE:2", "SEQUENCE:two")))!.event.canReply).toBe(false);
    expect(parseInvite(ics(base.replace("UID:abc-123@example.com", "UID: ")))!.event.canReply).toBe(false);
    expect(parseInvite(ics(base.replace("mailto:alice@example.com", "https://example.com")))!.event.canReply).toBe(false);
  });
});

describe("rsvpMail", () => {
  const event = parseInvite(ics(base))!.event;
  const now = new Date("2026-10-31T08:09:10.123Z");

  it("builds a folded RFC 5546 REPLY for the organizer", () => {
    const mail = rsvpMail(event, "bob@example.net", "TENTATIVE", now)!;
    expect(mail.to).toBe("alice@example.com");
    expect(mail.subject).toBe("Tentative: Planning, Q4");
    expect(mail.body).toBe("bob@example.net has tentatively accepted this invitation.");
    expect(mail.ics).toBe([
      "BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//KyPost//Web//EN", "METHOD:REPLY", "BEGIN:VEVENT",
      "UID:abc-123@example.com", "SEQUENCE:2", "DTSTAMP:20261031T080910Z", "ORGANIZER:mailto:alice@example.com",
      "ATTENDEE;PARTSTAT=TENTATIVE:mailto:bob@example.net", "END:VEVENT", "END:VCALENDAR", ""
    ].join("\r\n"));
  });

  it("folds long lines at 75 octets without splitting UTF-8", () => {
    const long = { ...event, uid: "é".repeat(100) };
    const lines = rsvpMail(long, "bob@example.net", "ACCEPTED", now)!.ics.split("\r\n");
    for (const line of lines) expect(new TextEncoder().encode(line).length).toBeLessThanOrEqual(75);
    expect(lines.join("\r\n").replace(/\r\n /g, "")).toContain(`UID:${long.uid}`);
  });

  it("refuses an attendee that could inject a line, and an unanswerable event", () => {
    expect(rsvpMail(event, "bob@example.net\r\nATTENDEE:x@y", "ACCEPTED", now)).toBeNull();
    expect(rsvpMail({ ...event, canReply: false }, "bob@example.net", "ACCEPTED", now)).toBeNull();
  });

  it("answers as the address the invite names, else the first", () => {
    expect(rsvpAttendee(event, ["me@example.org", "bob@example.net"])).toBe("bob@example.net");
    expect(rsvpAttendee(event, ["", "me@example.org"])).toBe("me@example.org");
  });
});

describe("answer flow", () => {
  const req = { messageId: "42", mailbox: "INBOX", index: 1, partstat: "ACCEPTED" as const };
  const refuse = (status: number, code?: string) => vi.fn().mockRejectedValue(new HttpError(`request failed: ${status}`, status, code ? { code, error: `says ${code}` } : undefined));

  it("is done when KyCalendar answered", async () => {
    const post = vi.fn().mockResolvedValue({ outcome: "answered" });
    expect(await answerThroughKyCalendar(post, req)).toEqual({ kind: "answered" });
    expect(post).toHaveBeenCalledWith("/api/calendar/rsvp", req);
  });

  it("maps kycalendar_off, conflict and unavailability", async () => {
    expect(await answerThroughKyCalendar(refuse(409, "kycalendar_off"), req)).toEqual({ kind: "off" });
    expect((await answerThroughKyCalendar(refuse(409, "conflict"), req)).kind).toBe("conflict");
    expect(await answerThroughKyCalendar(refuse(502, "kycalendar_unavailable"), req)).toEqual({ kind: "idle", message: "says kycalendar_unavailable" });
    expect((await answerThroughKyCalendar(vi.fn().mockRejectedValue(new TypeError("offline")), req)).kind).toBe("idle");
  });

  const mail = rsvpMail(parseInvite(ics(base))!.event, "bob@example.net", "ACCEPTED", new Date())!;

  it("counts only calendarReply: true as sent", async () => {
    const post = vi.fn().mockResolvedValue({ ok: true, calendarReply: true });
    expect(await answerByMail(post, mail, "bob@example.net")).toEqual({ kind: "sent" });
    expect(post).toHaveBeenCalledWith("/api/mail/send", expect.objectContaining({
      to: "alice@example.com", from: "bob@example.net", encrypt: false, sign: false, calendarReply: { ics: mail.ics }
    }));
    expect((await answerByMail(vi.fn().mockResolvedValue({ ok: true }), mail, "")).kind).toBe("plain");
  });

  it("re-arms on a 4xx or 503 and holds off when the send may have happened", async () => {
    expect((await answerByMail(refuse(400), mail, "")).kind).toBe("retry");
    expect((await answerByMail(refuse(503), mail, "")).kind).toBe("retry");
    expect((await answerByMail(refuse(502), mail, "")).kind).toBe("uncertain");
    expect((await answerByMail(vi.fn().mockRejectedValue(new TypeError("offline")), mail, "")).kind).toBe("uncertain");
  });
});
