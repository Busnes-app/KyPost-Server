import { describe, expect, it } from "vitest";
import { snoozePresets, sortTime } from "./snooze";

// Local-time fixtures: presets are defined in the user's own clock.
const local = (y: number, m: number, d: number, h: number, min = 0) => new Date(y, m - 1, d, h, min);
const presets = (now: Date) => Object.fromEntries(snoozePresets(now).map((p) => [p.label, p.at.getTime()]));

describe("snoozePresets", () => {
  it("offers later today while now + 3h is before 21:00", () => {
    // 2026-10-14 is a Wednesday.
    const now = local(2026, 10, 14, 17, 59);
    expect(presets(now)).toEqual({
      "Later today": local(2026, 10, 14, 20, 59).getTime(),
      "Tomorrow morning": local(2026, 10, 15, 8).getTime(),
      "Next week": local(2026, 10, 19, 8).getTime()
    });
  });

  it("drops later today at the evening cutoff", () => {
    expect(presets(local(2026, 10, 14, 18))).not.toHaveProperty("Later today");
    expect(presets(local(2026, 10, 14, 23, 30))).not.toHaveProperty("Later today");
  });

  it("goes a full week ahead on a Monday", () => {
    expect(presets(local(2026, 10, 12, 7))["Next week"]).toBe(local(2026, 10, 19, 8).getTime());
  });

  it("goes to tomorrow on a Sunday and across a month end", () => {
    const now = local(2026, 5, 31, 22);
    expect(presets(now)["Next week"]).toBe(local(2026, 6, 1, 8).getTime());
    expect(presets(now)["Tomorrow morning"]).toBe(local(2026, 6, 1, 8).getTime());
  });
});

describe("sortTime", () => {
  it("prefers the wake time", () => {
    expect(sortTime({ atUtc: "2026-01-01T00:00:00Z", wokenAt: "2026-10-12T09:00:00Z" })).toBe("2026-10-12T09:00:00Z");
  });

  it("falls back to arrival", () => {
    expect(sortTime({ atUtc: "2026-01-01T00:00:00Z" })).toBe("2026-01-01T00:00:00Z");
  });
});
