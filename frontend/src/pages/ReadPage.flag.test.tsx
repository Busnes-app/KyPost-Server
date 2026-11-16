import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor, within } from "@testing-library/react";
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
import { flagDueChoices } from "../lib/flag";

afterEach(cleanup);

const plans = { messageId: "11", sender: "b@example.com", subject: "Plans Mail", status: "read", atUtc: "2026-08-02T10:00:00Z" };
const other = { messageId: "12", sender: "c@example.com", subject: "Other Mail", status: "read", atUtc: "2026-08-01T10:00:00Z" };

function serve(flags: object[]) {
  getJSON.mockImplementation((url: string) => {
    if (url.startsWith("/api/inbox?")) return Promise.resolve({ tabs: ["Primary"], byTab: { Primary: [plans, other] }, cursor: 1 });
    if (url === "/api/mail/flags") return Promise.resolve({ flags });
    if (url === "/api/snoozes") return Promise.resolve({ snoozes: [] });
    return Promise.resolve({ configured: [], imap: [] });
  });
}

function renderPage() {
  render(
    <MemoryRouter initialEntries={["/"]}>
      <ReadPage />
    </MemoryRouter>
  );
}

describe("flag", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("flags with a due date, keeps the row and marks it", async () => {
    postJSON.mockResolvedValue({ ok: true, processed: 1, failed: [], tasks: { "11": "created" } });
    serve([]);
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByLabelText("Select email Plans Mail"));
    await user.click(screen.getByRole("button", { name: "Flag" }));
    await user.click(within(screen.getByRole("dialog", { name: "Flag" })).getByRole("button", { name: "Tomorrow" }));

    await waitFor(() => expect(postJSON).toHaveBeenCalled());
    const tomorrow = flagDueChoices(new Date()).find((c) => c.label === "Tomorrow")?.due;
    expect(postJSON.mock.calls[0]).toEqual(["/api/inbox/actions", expect.objectContaining({ action: "flag", messageIds: ["11"], mailbox: "", due: tomorrow })]);
    expect(await screen.findByText("Flagged")).toBeDefined();
    expect(screen.getByText("Plans Mail")).toBeDefined();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("sends no due date for No date and says when KyCalendar failed", async () => {
    postJSON.mockResolvedValue({ ok: true, processed: 1, failed: [], tasks: { "11": "failed" } });
    serve([]);
    const user = userEvent.setup();
    renderPage();
    await user.click(await screen.findByLabelText("Select email Plans Mail"));
    await user.click(screen.getByRole("button", { name: "Flag" }));
    await user.click(within(screen.getByRole("dialog", { name: "Flag" })).getByRole("button", { name: "No date" }));

    await waitFor(() => expect(postJSON).toHaveBeenCalled());
    expect((postJSON.mock.calls[0][1] as { due?: string }).due).toBeUndefined();
    expect(await screen.findByText(/KyCalendar could not make the task/)).toBeDefined();
    expect(screen.getByText("Flagged")).toBeDefined();
  });

  it("clears the flag of flagged messages", async () => {
    postJSON.mockResolvedValue({ ok: true, processed: 1, failed: [], tasks: { "11": "completed" } });
    serve([{ ref: "r", folder: "", messageId: "11", due: "" }]);
    const user = userEvent.setup();
    renderPage();
    await screen.findByText("Flagged");
    await user.click(await screen.findByLabelText("Select email Plans Mail"));
    await user.click(screen.getByRole("button", { name: "Clear flag" }));

    await waitFor(() => expect(postJSON).toHaveBeenCalled());
    expect(postJSON.mock.calls[0][1]).toMatchObject({ action: "unflag", messageIds: ["11"] });
    await waitFor(() => expect(screen.queryByText("Flagged")).toBeNull());
  });
});
