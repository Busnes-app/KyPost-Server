import { describe, expect, it } from "vitest";
import { groupRows, openTarget, rowCount, rowIds, rowNewest, rowSenders, type Conversation } from "./conversations";
import type { InboxEmail } from "./types";

const mail = (messageId: string, sender: string, atUtc: string, status = "read"): InboxEmail => ({ messageId, sender, subject: "Plan", status, atUtc });

const a = mail("1", "Alice <alice@example.com>", "2026-11-01T09:00:00Z");
const b = mail("2", "Bob <bob@example.com>", "2026-11-01T12:00:00Z", "unread");
const c = mail("3", "Alice <alice@example.com>", "2026-11-01T13:00:00Z");
const lone = mail("9", "Carol <carol@example.com>", "2026-11-01T11:00:00Z");
const conversation: Conversation = {
  id: "1",
  messageIds: ["1", "2", "3"],
  sent: [{ ...mail("s1", "Me <me@example.com>", "2026-11-01T14:00:00Z"), mailbox: "Sent" }]
};
const loaded = new Map([a, b, c, lone].map((m) => [m.messageId, m]));

describe("conversation rows", () => {
  // Newest first, as the list sorts by default.
  const rows = groupRows([c, b, lone, a], [conversation], loaded);

  it("collapses a conversation into the place of its first listed member", () => {
    expect(rows.map((r) => r.lead.messageId)).toEqual(["3", "9"]);
    expect(rowIds(rows[0])).toEqual(["1", "2", "3"]);
    expect(rowIds(rows[1])).toEqual(["9"]);
  });

  it("rolls up count, senders and newest time, replies from Sent included", () => {
    expect(rowCount(rows[0])).toBe(4);
    expect(rowSenders(rows[0])).toBe("Alice, Bob, me");
    expect(rowNewest(rows[0])).toBe("2026-11-01T14:00:00Z");
    expect(rowCount(rows[1])).toBe(1);
  });

  it("opens the newest unread message, else the newest", () => {
    expect(openTarget(rows[0]).messageId).toBe("2");
    expect(openTarget(groupRows([c, b, a], [conversation], new Map([a, { ...b, status: "read" }, c].map((m) => [m.messageId, m])))[0]).messageId).toBe("3");
  });

  it("acts only on members still listed", () => {
    const after = groupRows([c, a], [conversation], new Map([a, c].map((m) => [m.messageId, m])));
    expect(rowIds(after[0])).toEqual(["1", "3"]);
  });
});
