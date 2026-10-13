import { describe, expect, it } from "vitest";
import { schedulePresets, validSendAt } from "./schedule";

describe("schedulePresets", () => {
  it("offers an hour from now, tomorrow 08:00 and next Monday 08:00", () => {
    const wednesday = new Date(2026, 9, 14, 15, 30, 42);
    const [hour, tomorrow, monday] = schedulePresets(wednesday).map((p) => p.at);
    expect(hour).toEqual(new Date(2026, 9, 14, 16, 30, 0));
    expect(tomorrow).toEqual(new Date(2026, 9, 15, 8, 0, 0));
    expect(monday).toEqual(new Date(2026, 9, 19, 8, 0, 0));
  });

  it("on a Monday, Monday morning means next week", () => {
    const monday = new Date(2026, 9, 19, 7, 0, 0);
    expect(schedulePresets(monday)[2].at).toEqual(new Date(2026, 9, 26, 8, 0, 0));
  });

  it("every preset is a time the server accepts", () => {
    const now = new Date(2026, 9, 14, 23, 59, 30);
    for (const preset of schedulePresets(now)) expect(validSendAt(preset.at, now)).toBe(true);
  });
});

describe("validSendAt", () => {
  const now = new Date(2026, 0, 1, 12, 0, 0);
  it("needs at least a minute and at most 366 days", () => {
    expect(validSendAt(new Date(now.getTime() + 59_000), now)).toBe(false);
    expect(validSendAt(new Date(now.getTime() + 60_000), now)).toBe(true);
    expect(validSendAt(new Date(now.getTime() + 366 * 86_400_000), now)).toBe(true);
    expect(validSendAt(new Date(now.getTime() + 366 * 86_400_000 + 1), now)).toBe(false);
    expect(validSendAt(new Date("not a date"), now)).toBe(false);
  });
});
