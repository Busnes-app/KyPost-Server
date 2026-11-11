// The inbox refreshes on a 30 s timer only while the tab is visible, and at
// once when a hidden tab is shown again.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, cleanup, render } from "@testing-library/react";
import { MemoryRouter } from "react-router";

const getJSON = vi.fn();

vi.mock("../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  selectedMailboxId: () => "",
  postJSON: vi.fn(),
  downloadFile: vi.fn(),
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

let visibility: DocumentVisibilityState = "visible";
const inboxLoads = () => getJSON.mock.calls.filter(([url]) => String(url).startsWith("/api/inbox")).length;

function setVisibility(state: DocumentVisibilityState) {
  visibility = state;
  document.dispatchEvent(new Event("visibilitychange"));
}

beforeEach(() => {
  vi.useFakeTimers();
  vi.clearAllMocks();
  visibility = "visible";
  vi.spyOn(document, "visibilityState", "get").mockImplementation(() => visibility);
  getJSON.mockImplementation((url: string) =>
    Promise.resolve(url.startsWith("/api/inbox") ? { tabs: ["Primary"], byTab: { Primary: [] }, cursor: 1 } : {})
  );
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("ReadPage inbox refresh", () => {
  it("refreshes every 30 s while visible, pauses while hidden and refreshes on return", async () => {
    render(
      <MemoryRouter initialEntries={["/read"]}>
        <ReadPage />
      </MemoryRouter>
    );
    await act(async () => {});
    expect(inboxLoads()).toBe(1);

    await act(async () => vi.advanceTimersByTime(15_000));
    expect(inboxLoads()).toBe(1);
    await act(async () => vi.advanceTimersByTime(15_000));
    expect(inboxLoads()).toBe(2);

    act(() => setVisibility("hidden"));
    await act(async () => vi.advanceTimersByTime(120_000));
    expect(inboxLoads()).toBe(2);

    await act(async () => setVisibility("visible"));
    expect(inboxLoads()).toBe(3);
  });
});
