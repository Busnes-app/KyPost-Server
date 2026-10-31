import { describe, expect, it } from "vitest";
import { formatInboxListTime, senderName } from "./format";

describe("senderName", () => {
  it("shows the display name and drops the address", () => {
    expect(senderName("Alice Demo <alice@kypost-demo.local>")).toBe("Alice Demo");
    expect(senderName('"Ops, Dana" <dana@example.net>')).toBe("Ops, Dana");
  });

  it("falls back to the bare address", () => {
    expect(senderName("news@example.org")).toBe("news@example.org");
    expect(senderName("<news@example.org>")).toBe("news@example.org");
    expect(senderName("")).toBe("-");
  });
});

describe("formatInboxListTime", () => {
  it("uses a short month and day for older mail this year, and adds the year otherwise", () => {
    const now = new Date();
    const thisYear = new Date(now.getFullYear(), 0, 2, 9, 30).toISOString();
    const lastYear = new Date(now.getFullYear() - 1, 7, 11, 9, 30).toISOString();
    if (now.getMonth() > 0 || now.getDate() > 9) {
      expect(formatInboxListTime(thisYear)).toBe(new Date(thisYear).toLocaleDateString([], { month: "short", day: "numeric" }));
    }
    expect(formatInboxListTime(lastYear)).toContain(String(now.getFullYear() - 1));
  });
});
