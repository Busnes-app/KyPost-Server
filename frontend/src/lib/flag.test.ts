import { describe, expect, it } from "vitest";
import { flagDueChoices } from "./flag";

describe("flagDueChoices", () => {
  it("offers this week's Friday from Monday to Thursday", () => {
    // Wednesday 18 November 2026.
    expect(flagDueChoices(new Date(2026, 10, 18, 15))).toEqual([
      { label: "Today", due: "2026-11-18" },
      { label: "Tomorrow", due: "2026-11-19" },
      { label: "This week", due: "2026-11-20", date: expect.stringMatching(/ 20$/) },
      { label: "Next week", due: "2026-11-23", date: expect.stringMatching(/ 23$/) },
      { label: "No date", due: "" }
    ]);
  });
  it("drops this week from Friday on and crosses the month", () => {
    // Monday is next week; Saturday 28 November 2026.
    expect(flagDueChoices(new Date(2026, 10, 28, 9)).map((c) => c.due)).toEqual(["2026-11-28", "2026-11-29", "2026-11-30", ""]);
    // A Monday's next week is a full week on.
    expect(flagDueChoices(new Date(2026, 10, 16, 9)).find((c) => c.label === "Next week")?.due).toBe("2026-11-23");
  });
});
