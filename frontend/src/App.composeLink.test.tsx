// The compose deep link (lib/composeLink.ts): /compose opens a prefilled new
// draft once signed in, and waits through the sign-in when signed out.
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter, useLocation } from "react-router";
import { App } from "./App";
import { loadDraftSnapshot, saveDraftSnapshot } from "./app/draftAutosave";

vi.mock("./lib/pgpSession", () => ({
  subscribePGPSession: () => () => {},
  loadPGPSession: async () => ({ bootstrap: null, unlocked: false }),
  clearPGPSession: () => {},
  isClientProtected: () => false,
  isUnlocked: () => false,
  pgpCustody: () => "other",
  needsUnlock: () => false,
  accountAddress: () => "",
}));

const json = (status: number, body: unknown) => ({
  ok: status >= 200 && status < 300, status, headers: { get: () => "application/json" },
  json: async () => body, text: async () => JSON.stringify(body),
}) as unknown as Response;

let signedIn = true;
const sent = vi.fn();

beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn(async (url: string, init?: RequestInit) => {
    if (init?.method && init.method !== "GET") sent(url);
    if (url === "/api/auth/me") return json(200, signedIn ? { authenticated: true, userId: "u1", username: "gwen", role: "user" } : { authenticated: false });
    if (url === "/api/mailboxes") return json(200, { mailboxes: [] });
    if (url.startsWith("/api/inbox/folders")) return json(200, { folders: [] });
    return json(200, {});
  }));
});

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.sessionStorage.clear();
  sent.mockClear();
  signedIn = true;
});

function Where() {
  return <output data-testid="where">{useLocation().pathname}</output>;
}

const link = "/compose?" + new URLSearchParams({ to: "ann@example.com,not-an-address", subject: "Report", body: "<b>x</b> https://drive.example.com/l/#t" });

it("opens a prefilled new draft and sends or saves nothing", async () => {
  render(<MemoryRouter initialEntries={[link]}><App /><Where /></MemoryRouter>);
  expect(await screen.findByDisplayValue("Report", {}, { timeout: 5000 })).toBeTruthy();
  await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/read"));
  expect(screen.getByText("ann@example.com")).toBeTruthy();
  expect(screen.queryByText("not-an-address")).toBeNull();
  expect(sent.mock.calls.filter(([u]) => /\/api\/mail\/(send|draft)/.test(u))).toEqual([]);
  expect(window.sessionStorage.getItem("kypost.compose-link.v1")).toBeNull();
});

it("holds the link through the sign-in page", async () => {
  signedIn = false;
  render(<MemoryRouter initialEntries={[link]}><App /><Where /></MemoryRouter>);
  await waitFor(() => expect(screen.getByTestId("where").textContent).toBe("/login"), { timeout: 5000 });
  expect(JSON.parse(window.sessionStorage.getItem("kypost.compose-link.v1") ?? "{}")).toMatchObject({ to: ["ann@example.com"], subject: "Report" });
});

it("never autosaves over an unsent draft the tab already holds", async () => {
  const earlier = { to: "bob@example.com", cc: "", bcc: "", subject: "Half-written", body: "<p>keep me</p>", attachments: [], mailbox: "", from: "" };
  expect(await saveDraftSnapshot("u1", earlier)).toBe("saved");
  render(<MemoryRouter initialEntries={[link]}><App /><Where /></MemoryRouter>);
  expect(await screen.findByDisplayValue("Report", {}, { timeout: 5000 })).toBeTruthy();
  expect(screen.getByText(/Your earlier unsent draft is kept/)).toBeTruthy();
  // Past the one-second autosave, with no interaction.
  await new Promise((r) => setTimeout(r, 1600));
  expect(await loadDraftSnapshot("u1")).toMatchObject({ to: "bob@example.com", subject: "Half-written", body: "<p>keep me</p>" });
});

it("keeps a sealed draft it cannot open yet, byte for byte", async () => {
  // A sealed snapshot as client custody writes it; the vault is not open (and the PGP state not loaded).
  const sealed = JSON.stringify({ version: 2, savedAt: new Date().toISOString(), sealed: "-----BEGIN PGP MESSAGE-----\nx\n-----END PGP MESSAGE-----" });
  window.sessionStorage.setItem("kypost-compose-draft:u1", sealed);
  render(<MemoryRouter initialEntries={[link]}><App /><Where /></MemoryRouter>);
  expect(await screen.findByDisplayValue("Report", {}, { timeout: 5000 })).toBeTruthy();
  await new Promise((r) => setTimeout(r, 1600));
  expect(window.sessionStorage.getItem("kypost-compose-draft:u1")).toBe(sealed);
});
