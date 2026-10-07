import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { AuthContext, type AuthState } from "../../auth";
import { deriveCredential } from "../../api/auth";
import { withSSOStepUp } from "../../api/stepup";
import { MailExport } from "./MailExport";

vi.mock("../../api/auth", async (original) => ({
  ...await original<typeof import("../../api/auth")>(),
  deriveCredential: vi.fn(),
}));
vi.mock("../../api/stepup", () => ({ withSSOStepUp: vi.fn() }));

const token = "a".repeat(64);
const mailboxes = { mailboxes: [
  { id: "user-1", kind: "primary", addresses: [{ address: "me@example.test", kind: "primary" }] },
  { id: "mbx-1", kind: "extra", addresses: [{ address: "sales@example.test", kind: "primary" }] },
] };
let exportAnswer: () => Response;
const fetchMock = vi.fn<typeof fetch>();
const assign = vi.fn();
const user: AuthState = { authenticated: true, userId: "user-1", username: "me", role: "user" };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const posts = () => fetchMock.mock.calls.filter(([, init]) => init?.method === "POST");

function view(auth = user, entry = "/settings/mail?tab=export") {
  return render(<MemoryRouter initialEntries={[entry]}><AuthContext.Provider value={auth}><MailExport /></AuthContext.Provider></MemoryRouter>);
}
beforeEach(() => {
  exportAnswer = () => json({ url: `/api/export/${token}`, expiresInSeconds: 300, messages: 42 });
  vi.stubGlobal("fetch", fetchMock);
  vi.stubGlobal("location", { ...window.location, assign });
  fetchMock.mockImplementation(async (url, init) => {
    if (init?.method === "POST") return exportAnswer();
    if (url === "/api/mailboxes") return json(mailboxes);
    const selected = (init?.headers as Record<string, string>)["X-KyPost-Mailbox"];
    return json({ folders: selected === "mbx-1" ? ["INBOX", "Leads"] : ["INBOX", "Work"] });
  });
  vi.mocked(deriveCredential).mockResolvedValue({ password: "must-not-transmit", authSecret: "derived-test-secret", loginSalt: "salt", loginIterations: 1, derivation: "pbkdf2" });
  vi.mocked(withSSOStepUp).mockImplementation(run => run({}));
  document.cookie = "csrf_token=test-csrf";
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals(); document.cookie = "csrf_token=; Max-Age=0"; });

it("exports the chosen mailbox, folder and format after step-up, then navigates to the download", async () => {
  view();
  await screen.findByRole("option", { name: "Work" });
  expect(screen.getByText(/Encrypted messages stay encrypted/)).toBeTruthy();
  expect(screen.getByText(/holds your private mail/)).toBeTruthy();
  const button = screen.getByRole("button", { name: "Export mail" }) as HTMLButtonElement;
  expect(button.disabled).toBe(true);

  fireEvent.change(screen.getByLabelText("Mailbox"), { target: { value: "mbx-1" } });
  await screen.findByRole("option", { name: "Leads" });
  fireEvent.change(screen.getByLabelText("Folder"), { target: { value: "Leads" } });
  fireEvent.click(screen.getByLabelText(/zip of .eml files/));
  fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account-secret" } });
  fireEvent.click(button);

  await waitFor(() => expect(assign).toHaveBeenCalledWith(`/api/export/${token}`));
  expect(posts()).toHaveLength(1);
  const [url, init] = posts()[0]!;
  expect(url).toBe("/api/export");
  expect(JSON.parse(String(init!.body))).toEqual({ mailbox: "mbx-1", folder: "Leads", format: "eml-zip", authSecret: "derived-test-secret" });
  expect((init!.headers as Record<string, string>)["X-CSRF-Token"]).toBe("test-csrf");
  expect(vi.mocked(withSSOStepUp)).toHaveBeenCalledTimes(1);
  expect((screen.getByLabelText("Account password") as HTMLInputElement).value).toBe("");
  expect(await screen.findByText("Starting download of 42 messages…")).toBeTruthy();
});

it("confirms a KySignOn session through step-up without a password", async () => {
  view({ ...user, ssoSession: true });
  await screen.findByRole("option", { name: "Work" });
  expect(screen.queryByLabelText("Account password")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Export mail" }));
  await waitFor(() => expect(assign).toHaveBeenCalled());
  expect(JSON.parse(String(posts()[0]![1]!.body))).toEqual({ mailbox: "user-1", folder: "", format: "mbox" });
  expect(vi.mocked(deriveCredential)).not.toHaveBeenCalled();
});

it("refuses an unexpected download link and shows server refusals", async () => {
  exportAnswer = () => json({ url: "https://evil.example/x", messages: 1 });
  view({ ...user, ssoSession: true });
  await screen.findByRole("option", { name: "Work" });
  fireEvent.click(screen.getByRole("button", { name: "Export mail" }));
  expect((await screen.findByRole("alert")).textContent).toMatch(/unexpected download link/);
  exportAnswer = () => json({ url: `/api/export/${token}`, messages: "<img>" });
  fireEvent.click(screen.getByRole("button", { name: "Export mail" }));
  await waitFor(() => expect(screen.queryByText(/Starting download/)).toBeNull());
  await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/unexpected download link/));
  exportAnswer = () => json({ error: "too many attempts" }, 429);
  fireEvent.click(screen.getByRole("button", { name: "Export mail" }));
  await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/too many attempts/));
  expect(assign).not.toHaveBeenCalled();
});

it("explains export is for KyPost-hosted mailboxes when there are none", async () => {
  fetchMock.mockImplementation(async () => json({ mailboxes: [] }));
  view();
  expect(await screen.findByText(/use that provider's export/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Export mail" })).toBeNull();
});

it("explains a refused download and retries a busy one with the kept link", async () => {
  view(user, `/settings/mail?tab=export&export=busy&retry=${token}`);
  expect((await screen.findByRole("alert")).textContent).toMatch(/Another export is still running/);
  fireEvent.click(screen.getByRole("button", { name: "Retry" }));
  expect(assign).toHaveBeenCalledWith(`/api/export/${token}`);
  cleanup();
  view(user, "/settings/mail?tab=export&export=busy&retry=../../admin");
  await screen.findByRole("alert");
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
  cleanup();
  view(user, "/settings/mail?tab=export&export=proxy");
  expect((await screen.findByRole("alert")).textContent).toMatch(/proxy_http_version 1.1.*EML zip/);
  cleanup();
  view(user, `/settings/mail?tab=export&export=expired&retry=${token}`);
  expect((await screen.findByRole("alert")).textContent).toMatch(/expired or was already used/);
  expect(screen.queryByRole("button", { name: "Retry" })).toBeNull();
});
