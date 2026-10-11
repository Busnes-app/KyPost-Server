import { describe, expect, it } from "vitest";
import { safeWebsiteHref } from "./formState";

describe("safeWebsiteHref", () => {
  it("treats a scheme-less value as an https host, not a path on this origin", () => {
    expect(safeWebsiteHref("example.com")).toBe("https://example.com/");
    expect(safeWebsiteHref(" example.com/about ")).toBe("https://example.com/about");
    expect(safeWebsiteHref("/settings")).not.toContain(window.location.host);
  });

  it("keeps http(s) and refuses other schemes", () => {
    expect(safeWebsiteHref("http://example.com/x")).toBe("http://example.com/x");
    expect(safeWebsiteHref("javascript:alert(1)")).toBeUndefined();
    expect(safeWebsiteHref("data:text/html,x")).toBeUndefined();
  });
});
