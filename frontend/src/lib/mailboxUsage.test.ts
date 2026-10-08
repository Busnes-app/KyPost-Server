import { expect, it } from "vitest";
import { formatStorage, usageLevel, usageNotice, usageText } from "./mailboxUsage";

it("warns at 80% and is critical at 95% and beyond", () => {
  const q = 1000;
  expect([0, 799, 800, 949, 950, 1000, 1500].map(u => usageLevel(u, q))).toEqual(["ok", "ok", "warning", "warning", "critical", "critical", "critical"]);
  expect(usageLevel(0, 0)).toBe("critical");
  expect(usageNotice(799, q)).toBe("");
  expect(usageNotice(800, q)).toMatch(/over 80% full/);
  expect(usageNotice(950, q)).toMatch(/almost full/);
  expect(usageNotice(1000, q)).toMatch(/is full\. New mail is refused for now and senders retry/);
});

it("formats binary units like the server", () => {
  expect([0, 1023, 1024, 1536, 2 ** 20, 5 * 2 ** 30, 5.25 * 2 ** 30].map(b => formatStorage(b))).toEqual(["0 B", "1023 B", "1 KiB", "1.5 KiB", "1 MiB", "5 GiB", "5.3 GiB"]);
  expect(usageText(4 * 2 ** 30, 5 * 2 ** 30)).toBe("4 GiB of 5 GiB used (80%)");
  expect(usageText(6 * 2 ** 30, 5 * 2 ** 30)).toBe("6 GiB of 5 GiB used (120%)");
  // Short of the quota never reads as full.
  expect(usageText(Math.round(4.96 * 2 ** 30), 5 * 2 ** 30)).toBe("4.9 GiB of 5 GiB used (99%)");
  expect(usageText(5 * 2 ** 30 - 1, 5 * 2 ** 30)).toBe("4.9 GiB of 5 GiB used (99%)");
  expect(formatStorage(Math.round(4.96 * 2 ** 30), "down")).toBe("4.9 GiB");
});
