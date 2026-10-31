import { describe, expect, it } from "vitest";
import { visibleSettingsTabs } from "./navigation";

describe("visibleSettingsTabs", () => {
  it("puts the personal sections first, then KyPost's own, then Status for a non-admin", () => {
    expect(visibleSettingsTabs(false).map((t) => t.label)).toEqual([
      "Phones & apps",
      "Appearance",
      "Mail",
      "Security",
      "Notifications",
      "Email Labels",
      "Status"
    ]);
  });

  it("gives an admin Server and Diagnostics instead of Status", () => {
    const labels = visibleSettingsTabs(true).map((t) => t.label);
    expect(labels.slice(-2)).toEqual(["Server", "Diagnostics"]);
    expect(labels).not.toContain("Status");
  });

  it("drops the Login entry, which redirected signed-in users to the inbox", () => {
    for (const isAdmin of [true, false]) {
      const routes = visibleSettingsTabs(isAdmin).map((t) => t.to);
      expect(routes).not.toContain("/login");
      expect(routes).not.toContain("/password");
    }
  });

  it("routes every item under /settings or /admin", () => {
    for (const isAdmin of [true, false]) {
      for (const tab of visibleSettingsTabs(isAdmin)) {
        expect(tab.to).toMatch(/^\/(settings|admin)\//);
      }
    }
  });

});

// The Settings tabs render whatever this module returns, so an entry with no matching
// route in App.tsx is a dead link — which is exactly what shipped once during
// this restructure, when the nav pointed at panels that did not exist yet.
// Reading the router source is blunt, but it is the only thing that actually
// couples the two files.
describe("every nav target is routed", () => {
  it("has a Route in App.tsx for each item both roles can see", async () => {
    const { readFileSync } = await import("node:fs");
    // Relative to the vitest root (frontend/), not this file: import.meta.url
    // is not a file: URL under the jsdom environment.
    const app = readFileSync("src/App.tsx", "utf8");

    const targets = new Set(
      [true, false].flatMap((isAdmin) =>
        visibleSettingsTabs(isAdmin).map((tab) => tab.to)
      )
    );

    for (const target of targets) {
      expect(app, `no <Route path="${target}"> in App.tsx`).toContain(`path="${target}"`);
    }
  });
});

// Email Labels sits under Config, not Admin. Everything on it is per-user —
// every signed-in user has their own TUNING.md and decision log, and those
// endpoints are withAuth — so an admin-only entry would have locked users out
// of their own settings. Nothing on the panel is role-gated.
describe("Email Labels placement", () => {
  it("is reachable by a non-admin", () => {
    const routes = visibleSettingsTabs(false).map((t) => t.to);
    expect(routes).toContain("/settings/automation");
  });

  it("is not under /admin, which would imply a gate it does not have", () => {
    for (const isAdmin of [true, false]) {
      const automation = visibleSettingsTabs(isAdmin).find((t) => t.label === "Email Labels");
      expect(automation?.to).toBe("/settings/automation");
    }
  });
});
