import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ScheduledPage } from "./ScheduledPage";
import { OUTER_PLACEHOLDER_SUBJECT } from "../lib/pgpClient";

const waiting = { id: "row-1", status: "scheduled", reason: "", sendAt: "2026-11-02T09:30:00Z", subject: "quarterly report", to: ["bob@example.com", "carol@example.com"] };
const returned = { id: "row-2", status: "returned", reason: "your PGP keys changed; review it and send again", sendAt: "2026-11-03T10:00:00Z", subject: OUTER_PLACEHOLDER_SUBJECT, to: ["dave@example.com"] };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const fetchMock = vi.fn<typeof fetch>();
let list: unknown[];
let cancelAnswer: () => Response;
const lists = () => fetchMock.mock.calls.filter(([url, init]) => url === "/api/scheduled" && !init?.method);

beforeEach(() => {
  list = [waiting, returned];
  cancelAnswer = () => json({ ok: true, status: "returned" });
  vi.stubGlobal("fetch", fetchMock);
  fetchMock.mockImplementation(async (url, init) => (init?.method === "POST" ? cancelAnswer() : json({ scheduled: list })));
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals(); });

describe("ScheduledPage", () => {
  it("lists each row's send time, recipients, subject, status and reason", async () => {
    render(<ScheduledPage />);
    const first = (await screen.findByText("quarterly report")).closest("tr")!;
    expect(first.textContent).toContain(new Date(waiting.sendAt).toLocaleString());
    expect(first.textContent).toContain("bob@example.com, carol@example.com");
    expect(first.textContent).toContain("Waiting");
    const second = screen.getByText("dave@example.com").closest("tr")!;
    expect(second.textContent).toContain("(encrypted)");
    expect(second.textContent).toContain("Returned to Drafts");
    expect(second.textContent).toContain(returned.reason);
    expect(screen.getAllByRole("button", { name: "Cancel" })).toHaveLength(1);
  });

  it("cancels through the API and refreshes the list", async () => {
    render(<ScheduledPage />);
    await screen.findByText("quarterly report");
    list = [{ ...waiting, status: "returned", reason: "canceled" }, returned];
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    const post = fetchMock.mock.calls.find(([, init]) => init?.method === "POST");
    expect(post?.[0]).toBe("/api/scheduled/row-1/cancel");
    await waitFor(() => expect(lists()).toHaveLength(2));
    expect((await screen.findByText("canceled")).closest("tr")!.textContent).toContain("Returned to Drafts");
    expect(screen.queryByRole("button", { name: "Cancel" })).toBeNull();
  });

  it("shows the server's refusal when the row is already sending", async () => {
    cancelAnswer = () => json({ error: "this message is no longer waiting and cannot be canceled", status: "sending" }, 409);
    render(<ScheduledPage />);
    await screen.findByText("quarterly report");
    list = [{ ...waiting, status: "sending" }, returned];
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(await screen.findByText(/no longer waiting and cannot be canceled/)).toBeTruthy();
    await waitFor(() => expect(lists()).toHaveLength(2));
    expect((await screen.findByText("Sending")).closest("tr")!.textContent).toContain("quarterly report");
  });
});
