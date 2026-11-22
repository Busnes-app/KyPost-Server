import { describe, expect, it } from "vitest";
import { pickupFallbackFlag, serializeRecipientField, splitAddressList } from "./recipients";

describe("pickupFallbackFlag", () => {
  it("is false when not encrypting, whatever the checkbox says", () => {
    expect(pickupFallbackFlag(false, true)).toBe(false);
  });

  it("is false when encrypting without the opt-in", () => {
    expect(pickupFallbackFlag(true, false)).toBe(false);
  });

  it("is true only when encrypting and opted in", () => {
    expect(pickupFallbackFlag(true, true)).toBe(true);
  });
});

describe("a pasted list left in the draft", () => {
  it("is sent as separate addresses, split on commas and semicolons", () => {
    const field = { tokens: [{ email: "a@b.test", isCustom: false }], draft: " c@d.test, e@f.test; g@h.test " };
    expect(splitAddressList(serializeRecipientField(field))).toEqual(["a@b.test", "c@d.test", "e@f.test", "g@h.test"]);
  });
});
