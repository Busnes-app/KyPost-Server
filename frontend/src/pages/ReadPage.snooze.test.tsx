import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";

const getJSON = vi.fn();
const postJSON = vi.fn();

vi.mock("../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  selectedMailboxId: () => "",
  postJSON: (url: string, body: unknown) => postJSON(url, body),
  toErrorMessage: (_e: unknown, fallback: string) => fallback
}));

vi.mock("../api/pgp", () => ({ getPGPMessagePayload: vi.fn() }));
vi.mock("../lib/pgpClient", () => ({ decryptMessage: vi.fn(), verifySignedMessage: vi.fn() }));
vi.mock("../lib/pgpSession", () => ({
  isClientProtected: () => false,
  needsUnlock: () => false,
  subscribePGPSession: () => () => {}
}));
vi.mock("../components/PgpUnlockDialog", () => ({ PgpUnlockDialog: () => null }));

import { ReadPage } from "./ReadPage";

afterEach(cleanup);

const newer = { messageId: "11", sender: "b@example.com", subject: "Newer Mail", status: "read", atUtc: "2026-08-02T10:00:00Z" };
const woken = {
  messageId: "10",
  sender: "a@example.com",
  subject: "Woken Mail",
  status: "read",
  atUtc: "2026-08-01T10:00:00Z",
  wokenAt: "2026-08-03T08:00:00Z"
};

// Only the first list load answers, so a row that leaves did so without a reload.
function serve(rows: object[], snoozes: object[] = []) {
  let loads = 0;
  getJSON.mockImplementation((url: string) => {
    if (url.startsWith("/api/inbox?")) {
      return ++loads > 1 ? new Promise(() => {}) : Promise.resolve({ tabs: ["Primary"], byTab: { Primary: rows }, cursor: 1 });
    }
    if (url === "/api/snoozes") return Promise.resolve({ snoozes });
    return Promise.resolve({ configured: [], imap: [] });
  });
}

function renderAt(path = "/") {
  render(
    <MemoryRouter initialEntries={[path]}>
      <ReadPage />
    </MemoryRouter>
  );
}

const subjects = () =>
  [...document.querySelectorAll(".inbox-subject")].map((el) => el.textContent ?? "").filter((t) => t.endsWith(" Mail"));

describe("snooze", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    postJSON.mockResolvedValue({ ok: true, processed: 1, failed: [] });
  });

  it("sorts a woken email by its wake time and marks it", async () => {
    serve([newer, woken]);
    renderAt();
    await screen.findByText("Woken Mail");
    expect(subjects()).toEqual(["Woken Mail", "Newer Mail"]);
    expect(screen.getAllByText("Snoozed").length).toBeGreaterThan(0);
  });

  it("sends a preset as an ISO wakeAt and drops the row", async () => {
    serve([newer, woken]);
    const user = userEvent.setup();
    renderAt();
    await user.click(await screen.findByLabelText("Select email Newer Mail"));
    await user.click(screen.getByRole("button", { name: "Snooze" }));
    await user.click(within(screen.getByRole("dialog", { name: "Snooze" })).getByRole("button", { name: "Tomorrow morning" }));

    await waitFor(() => expect(postJSON).toHaveBeenCalled());
    const [url, body] = postJSON.mock.calls[0];
    expect(url).toBe("/api/inbox/actions");
    expect(body).toMatchObject({ action: "snooze", messageIds: ["11"], mailbox: "" });
    const wakeAt = (body as { wakeAt: string }).wakeAt;
    expect(new Date(wakeAt).toISOString()).toBe(wakeAt);
    expect(new Date(wakeAt).getHours()).toBe(8);
    await waitFor(() => expect(screen.queryByText("Newer Mail")).toBeNull());
  });

  it("converts a picked local time to ISO", async () => {
    serve([newer]);
    const user = userEvent.setup();
    renderAt();
    await user.click(await screen.findByLabelText("Select email Newer Mail"));
    await user.click(screen.getByRole("button", { name: "Snooze" }));
    const dialog = within(screen.getByRole("dialog", { name: "Snooze" }));
    fireEvent.change(dialog.getByLabelText("Pick a date and time"), { target: { value: "2099-03-04T09:30" } });
    await user.click(dialog.getByRole("button", { name: "Snooze" }));

    await waitFor(() => expect(postJSON).toHaveBeenCalled());
    expect(postJSON.mock.calls[0][1]).toMatchObject({ action: "snooze", wakeAt: new Date("2099-03-04T09:30").toISOString() });
  });

  it("keeps the row and reports a per-message failure", async () => {
    postJSON.mockResolvedValue({ ok: true, processed: 0, failed: [{ messageId: "11", error: "no Message-ID" }] });
    serve([newer]);
    const user = userEvent.setup();
    renderAt();
    await user.click(await screen.findByLabelText("Select email Newer Mail"));
    await user.click(screen.getByRole("button", { name: "Snooze" }));
    await user.click(within(screen.getByRole("dialog", { name: "Snooze" })).getByRole("button", { name: "Tomorrow morning" }));

    await screen.findByText(/Inbox action failed/);
    expect(screen.getByText("Newer Mail")).toBeDefined();
  });

  it("shows when each row of the Snoozed folder wakes", async () => {
    serve([newer], [{ messageId: "11", wakeAt: "2099-03-04T09:30:00Z" }]);
    renderAt("/?mailbox=Snoozed");
    await screen.findByText("Newer Mail");
    expect((await screen.findAllByText(/^until /)).length).toBeGreaterThan(0);
  });
});
