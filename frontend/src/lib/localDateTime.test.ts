import { expect, it } from "vitest";
import { localDateTimeValue } from "./localDateTime";

it("localDateTimeValue round-trips through a datetime-local input", () => {
  const at = new Date(2026, 4, 6, 7, 8, 0);
  expect(localDateTimeValue(at)).toBe("2026-05-06T07:08");
  expect(new Date(localDateTimeValue(at))).toEqual(at);
});
