// The phone navigation strip's "Settings" button must actually reveal the
// settings links. On a phone the sidebar is a short scrolling strip and the
// links render at its bottom, so tapping the button looked like it did nothing
// (#456). Opening them must scroll the group into view and mark the control
// expanded.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { App } from "./App";

vi.mock("./lib/pgpSession", () => ({
  subscribePGPSession: () => () => {},
  loadPGPSession: async () => ({ bootstrap: null, unlocked: false }),
  clearPGPSession: () => {},
  isClientProtected: () => false,
  pgpCustody: () => "other",
  needsUnlock: () => false,
  accountAddress: () => "",
}));

function json(status: number, body: unknown) {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: { get: () => "application/json" },
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response;
}

let originalScrollIntoView: typeof Element.prototype.scrollIntoView;

beforeEach(() => {
  originalScrollIntoView = Element.prototype.scrollIntoView;
  vi.stubGlobal("fetch", vi.fn(async (url: string) => {
    if (url === "/api/auth/me") return json(200, { authenticated: true, userId: "u1", username: "gwen", role: "user" });
    if (url === "/api/mailboxes") return json(200, { mailboxes: [] });
    if (url.startsWith("/api/inbox/folders")) return json(200, { folders: [] });
    if (url.startsWith("/api/inbox?")) {
      return json(200, {
        tabs: ["Primary"],
        byTab: { Primary: [{ messageId: "m1", sender: "a@b.test", subject: "Primary inbox", status: "read", atUtc: "2026-10-01T10:00:00Z" }] },
        cursor: 1,
      });
    }
    return json(200, {});
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  Element.prototype.scrollIntoView = originalScrollIntoView;
  window.sessionStorage.clear();
});

describe("mobile Settings button", () => {
  it("opens the settings links, scrolls them into view, and marks itself expanded", async () => {
    const scrollIntoView = vi.fn();
    Element.prototype.scrollIntoView = scrollIntoView;
    const user = userEvent.setup();

    render(<MemoryRouter initialEntries={["/read"]}><App /></MemoryRouter>);
    // The first render in this file measures about 1.1s under parallel test load (12 timed
    // runs), past findBy's 1s default. This asserts mailbox content, not render latency.
    await screen.findByText("Primary inbox", {}, { timeout: 5000 });

    const toggle = screen.getByRole("button", { name: "Toggle settings" });
    // The current-page link is scrolled into view on load; only the toggle's scroll counts here.
    scrollIntoView.mockClear();
    // The phone strip follows sidebar order: the toggle comes after Contacts.
    const contacts = screen.getByRole("link", { name: "Contacts" });
    expect(contacts.compareDocumentPosition(toggle) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    // Closed to start: no settings link, and the control says so.
    expect(toggle.getAttribute("aria-expanded")).toBe("false");
    expect(screen.queryByRole("link", { name: "Appearance" })).toBeNull();

    await user.click(toggle);

    // The links are now present, the control is expanded, and the group was
    // scrolled into view (the fix for the hidden 24vh strip).
    expect(await screen.findByRole("link", { name: "Appearance" })).toBeTruthy();
    expect(toggle.getAttribute("aria-expanded")).toBe("true");
    expect(scrollIntoView.mock.contexts).toContain(document.getElementById("settings-nav-group"));
  });
});
