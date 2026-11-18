import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { EncryptionSetupPage } from "./EncryptionSetupPage";
import type { AuthState, EncryptionSetup } from "../auth";
import { setupDismissed } from "../lib/setupDismissal";

const auth = vi.hoisted(() => ({ value: {} as AuthState }));
vi.mock("../auth", () => ({ useAuth: () => auth.value }));
vi.mock("./security/sections/MailKeys", () => ({
  MailKeys: (props: { incomingDefaultOn?: boolean }) => <p>mail keys, incoming default {String(props.incomingDefaultOn)}</p>,
}));
vi.mock("../components/PgpUnlockDialog", () => ({ PgpUnlockDialog: () => null }));
vi.mock("../api/pgp", () => ({ getPGPIdentity: vi.fn().mockResolvedValue(null) }));
vi.mock("../lib/pgpSession", () => ({ loadPGPSession: vi.fn().mockResolvedValue(undefined), subscribePGPSession: () => () => {} }));

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

beforeEach(() => { localStorage.clear(); });
afterEach(cleanup);

it("required setup cannot be skipped or finished without recovery", async () => {
  auth.value = { authenticated: true, userId: "u1", encryptionSetup: { ...base, hasKey: true } };
  const refresh = renderPage();
  expect(screen.queryByRole("button", { name: "Not now" })).toBeNull();
  expect(screen.getByText(/mail keys, incoming default true/)).toBeTruthy();
  expect(screen.getByText(/Mail is encrypted to your key as soon as it arrives/)).toBeTruthy();
  expect(screen.getByText(/we can't stop the sending server from seeing it/)).toBeTruthy();
  expect(screen.getByText(/Search, filters and AI labels can only use what we see before encrypting \(headers\)/)).toBeTruthy();
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
