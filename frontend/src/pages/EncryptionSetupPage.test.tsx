import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { EncryptionSetupPage } from "./EncryptionSetupPage";
import type { AuthState, EncryptionSetup } from "../auth";
import { setupDismissed } from "../lib/setupDismissal";

const auth = vi.hoisted(() => ({ value: {} as AuthState }));
vi.mock("../auth", () => ({ useAuth: () => auth.value }));
vi.mock("./security/sections/MailKeys", () => ({
  MailKeys: (props: { incomingDefaultOn?: boolean; onIncomingSaved?: () => void }) => (
    <p>mail keys, incoming default {String(props.incomingDefaultOn)} <button type="button" onClick={props.onIncomingSaved}>save incoming</button></p>
  ),
}));
vi.mock("../settings/sections/EmailServer", () => ({
  EmailServer: (props: { onSaved?: () => void }) => <button type="button" onClick={props.onSaved}>save mail account</button>,
}));
vi.mock("../components/PgpUnlockDialog", () => ({ PgpUnlockDialog: () => null }));
vi.mock("../api/pgp", () => ({ getPGPIdentity: vi.fn().mockResolvedValue(null) }));
const pgp = vi.hoisted(() => ({ listener: null as ((state: unknown) => void) | null, load: vi.fn() }));
vi.mock("../lib/pgpSession", () => ({
  loadPGPSession: () => { pgp.load(); return Promise.resolve(undefined); },
  subscribePGPSession: (listener: (state: unknown) => void) => { pgp.listener = listener; return () => {}; }
}));
const emitSession = (pgpRevision: number, suggestedUserIDs = ["ana@example.com"]) =>
  act(() => pgp.listener?.({ bootstrap: { pgpRevision, suggestedUserIDs } }));

const base: EncryptionSetup = { required: true, offered: false, hasKey: false, hasRecovery: false, incomingEnabled: false, keyPolicy: "required", incomingPolicy: "on" };

function renderPage(onRefresh = vi.fn().mockResolvedValue(undefined)) {
  render(
    <MemoryRouter initialEntries={["/setup/encryption"]}>
      <Routes>
        <Route path="/setup/encryption" element={<EncryptionSetupPage onRefresh={onRefresh} />} />
        <Route path="/read" element={<p>inbox</p>} />
      </Routes>
    </MemoryRouter>
  );
  return onRefresh;
}

beforeEach(() => { localStorage.clear(); pgp.load.mockClear(); });
afterEach(cleanup);

it("required setup cannot be skipped or finished without recovery", async () => {
  auth.value = { authenticated: true, userId: "u1", encryptionSetup: { ...base, hasKey: true } };
  const refresh = renderPage();
  expect(screen.queryByRole("button", { name: "Not now" })).toBeNull();
  expect(screen.getByText(/mail keys, incoming default true/)).toBeTruthy();
  expect(screen.getByText(/Mail is encrypted to your key as soon as it arrives/)).toBeTruthy();
  expect(screen.getByText(/we can't stop the sending server from seeing it/)).toBeTruthy();
  expect(screen.getByText(/Server search and body filter rules can only use headers once mail is encrypted; AI labels are applied as mail arrives, before it is encrypted\./)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Finish setup" }));
  expect((await screen.findByRole("alert")).textContent).toMatch(/Store a recovery copy first/);
  expect(refresh).toHaveBeenCalled();
  expect(screen.queryByText("inbox")).toBeNull();
});

it("finishes once the server reports setup complete", async () => {
  auth.value = { authenticated: true, userId: "u1", encryptionSetup: { ...base, required: false, hasKey: true, hasRecovery: true } };
  renderPage();
  fireEvent.click(screen.getByRole("button", { name: "Finish setup" }));
  expect(await screen.findByText("inbox")).toBeTruthy();
});

it("an optional offer can be put off, once per browser", async () => {
  auth.value = { authenticated: true, userId: "u2", encryptionSetup: { ...base, required: false, offered: true, keyPolicy: "optional", incomingPolicy: "off" } };
  renderPage();
  expect(screen.getByText(/incoming default false/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Not now" }));
  expect(await screen.findByText("inbox")).toBeTruthy();
  expect(setupDismissed("u2")).toBe(true);
});

it("re-reads setup as each step completes, not only on Finish", async () => {
  auth.value = { authenticated: true, userId: "u1", encryptionSetup: base };
  const refresh = renderPage();
  emitSession(1);
  expect(refresh).toHaveBeenCalledTimes(1);
  emitSession(1);
  expect(refresh).toHaveBeenCalledTimes(1);
  emitSession(2); // key created, or recovery copy stored
  expect(refresh).toHaveBeenCalledTimes(2);
  fireEvent.click(screen.getByRole("button", { name: "save incoming" }));
  expect(refresh).toHaveBeenCalledTimes(3);
});

it("sets up a missing mail account inside setup, then reloads the key's addresses", async () => {
  auth.value = { authenticated: true, userId: "u1", encryptionSetup: base };
  renderPage();
  emitSession(1, []);
  expect(screen.getByRole("heading", { name: "Set up your mail account" })).toBeTruthy();
  const loads = pgp.load.mock.calls.length;
  fireEvent.click(screen.getByRole("button", { name: "save mail account" }));
  expect(pgp.load.mock.calls.length).toBe(loads + 1);
  emitSession(1);
  expect(screen.queryByRole("heading", { name: "Set up your mail account" })).toBeNull();
  expect(screen.getByRole("heading", { name: "Set up email encryption" })).toBeTruthy();
});
