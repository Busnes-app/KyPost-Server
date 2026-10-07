import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { AuthContext, type AuthState } from "../../auth";
import { deriveCredential } from "../../api/auth";
import { withSSOStepUp } from "../../api/stepup";
import { MailImport } from "./MailImport";

vi.mock("../../api/auth", async (original) => ({
  ...await original<typeof import("../../api/auth")>(),
  deriveCredential: vi.fn(),
}));
vi.mock("../../api/stepup", () => ({ withSSOStepUp: vi.fn() }));

const token = "b".repeat(64);
const mailboxes = { mailboxes: [
  { id: "user-1", kind: "primary", addresses: [{ address: "me@example.test", kind: "primary" }] },
  { id: "mbx-1", kind: "extra", addresses: [{ address: "sales@example.test", kind: "primary" }] },
] };
const idle = { state: "idle", imported: 0, duplicates: 0, skipped: 0, bytes: 0, maxBytes: 32 << 20, maxMessageBytes: 5 << 20 };
const running = { ...idle, state: "running", folder: "Old mail", imported: 1 };
let statuses: unknown[];
let startAnswer: () => Response;
const fetchMock = vi.fn<typeof fetch>();
const user: AuthState = { authenticated: true, userId: "user-1", username: "me", role: "user" };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const posts = () => fetchMock.mock.calls.filter(([, init]) => init?.method === "POST");

// FakeXHR records the upload and lets the test drive progress and the answer.
class FakeXHR {
  static last: FakeXHR | null = null;
  method = ""; url = ""; headers: Record<string, string> = {}; body: unknown; withCredentials = false;
  status = 0; responseText = ""; aborted = false;
  upload: { onprogress: ((e: { loaded: number; total: number; lengthComputable: boolean }) => void) | null } = { onprogress: null };
  onload: (() => void) | null = null; onerror: (() => void) | null = null; onabort: (() => void) | null = null;
  constructor() { FakeXHR.last = this; }
  open(method: string, url: string) { this.method = method; this.url = url; }
  setRequestHeader(k: string, v: string) { this.headers[k] = v; }
  send(body: unknown) { this.body = body; }
  abort() { this.aborted = true; this.onabort?.(); }
  answer(status: number, body: unknown) { this.status = status; this.responseText = JSON.stringify(body); this.onload?.(); }
}

function view(auth = user) {
  return render(<AuthContext.Provider value={auth}><MailImport /></AuthContext.Provider>);
}
async function chooseFile(name = "Inbox", content = "From a b\r\nSubject: x\r\n\r\nx\r\n") {
  const file = new File([content], name);
  fireEvent.change(await screen.findByLabelText("File"), { target: { files: [file] } });
  return file;
}
beforeEach(() => {
  statuses = [idle];
  startAnswer = () => json({ url: `/api/import/${token}`, expiresInSeconds: 300, folder: "Old mail", maxBytes: 32 << 20 });
  FakeXHR.last = null;
  vi.stubGlobal("fetch", fetchMock);
  vi.stubGlobal("XMLHttpRequest", FakeXHR);
  fetchMock.mockImplementation(async (url, init) => {
    if (url === "/api/import" && init?.method === "POST") return startAnswer();
    if (url === "/api/import/cancel") return json({ ...running, state: "running" });
    if (url === "/api/import") return json(statuses.length > 1 ? statuses.shift() : statuses[0]);
    if (url === "/api/mailboxes") return json(mailboxes);
    return json({ folders: ["INBOX", "Work"] });
  });
  vi.mocked(deriveCredential).mockResolvedValue({ password: "must-not-transmit", authSecret: "derived-test-secret", loginSalt: "salt", loginIterations: 1, derivation: "pbkdf2" });
  vi.mocked(withSSOStepUp).mockImplementation(run => run({}));
  document.cookie = "csrf_token=test-csrf";
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals(); document.cookie = "csrf_token=; Max-Age=0"; });

it("explains the trade-offs, steps up, uploads with progress, then polls the counts", async () => {
  view();
  expect(await screen.findByText(/not scanned for spam, not run through your rules/)).toBeTruthy();
  expect(screen.getByText(/skips the messages already there/)).toBeTruthy();
  expect(await screen.findByText(/up to 32 MiB .* over 5 MiB is skipped/)).toBeTruthy();
  expect((screen.getByLabelText("File") as HTMLInputElement).accept).toBe("");
  const button = screen.getByRole("button", { name: "Import mail" }) as HTMLButtonElement;
  expect(button.disabled).toBe(true);

  fireEvent.change(screen.getByLabelText("Mailbox"), { target: { value: "mbx-1" } });
  fireEvent.change(screen.getByLabelText("Folder"), { target: { value: "Old mail" } });
  const file = await chooseFile();
  fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account-secret" } });
  statuses = [running, { ...idle, state: "finished", folder: "Old mail", imported: 3, duplicates: 2, skipped: 1, bytes: 900 }];
  fireEvent.click(button);

  await waitFor(() => expect(FakeXHR.last).not.toBeNull());
  const xhr = FakeXHR.last!;
  expect(JSON.parse(String(posts()[0]![1]!.body))).toEqual({ mailbox: "mbx-1", folder: "Old mail", authSecret: "derived-test-secret" });
  expect((screen.getByLabelText("Account password") as HTMLInputElement).value).toBe("");
  expect([xhr.method, xhr.url, xhr.body, xhr.withCredentials]).toEqual(["POST", `/api/import/${token}`, file, true]);
  expect(xhr.headers).toEqual({ "X-CSRF-Token": "test-csrf", "Content-Type": "application/octet-stream" });

  xhr.upload.onprogress!({ loaded: 50, total: 200, lengthComputable: true });
  expect(await screen.findByText(/25%/)).toBeTruthy();
  expect((screen.getByLabelText("Upload progress") as HTMLProgressElement).value).toBe(50);
  xhr.answer(202, running);
  expect(await screen.findByText(/Importing into Old mail: 1 message imported/)).toBeTruthy();
  expect(await screen.findByText(/Import finished into Old mail: 3 messages imported, 2 duplicates skipped, 1 could not be imported/, {}, { timeout: 5000 })).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Cancel import" })).toBeNull();
});

it("cancels a running import and shows an upload refusal", async () => {
  statuses = [running];
  view({ ...user, ssoSession: true });
  fireEvent.click(await screen.findByRole("button", { name: "Cancel import" }));
  await waitFor(() => expect(posts().map(([url]) => url)).toEqual(["/api/import/cancel"]));
  expect(posts()[0]![1]!.headers).toMatchObject({ "X-CSRF-Token": "test-csrf" });
  cleanup();

  statuses = [idle];
  view({ ...user, ssoSession: true });
  expect(screen.queryByLabelText("Account password")).toBeNull();
  await chooseFile("mail.zip");
  fireEvent.click(screen.getByRole("button", { name: "Import mail" }));
  await waitFor(() => expect(FakeXHR.last).not.toBeNull());
  expect(vi.mocked(deriveCredential)).not.toHaveBeenCalled();
  statuses = [{ ...idle, state: "failed", error: "incoming encryption is on" }];
  FakeXHR.last!.answer(409, { error: "import is unavailable while incoming encryption is on" });
  expect((await screen.findByText(/request failed: 409 - import is unavailable/)).getAttribute("role")).toBe("alert");
});

it("aborts the upload on cancel and refuses an unexpected link or an oversized file", async () => {
  view({ ...user, ssoSession: true });
  await chooseFile();
  fireEvent.click(screen.getByRole("button", { name: "Import mail" }));
  await waitFor(() => expect(FakeXHR.last).not.toBeNull());
  fireEvent.click(screen.getByRole("button", { name: "Cancel upload" }));
  expect(FakeXHR.last!.aborted).toBe(true);
  expect(await screen.findByText("Upload cancelled. Nothing was imported.")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Cancel upload" })).toBeNull();
  expect(screen.queryByRole("alert")).toBeNull();

  FakeXHR.last = null;
  startAnswer = () => json({ url: "https://evil.example/x", maxBytes: 1 });
  fireEvent.click(screen.getByRole("button", { name: "Import mail" }));
  expect((await screen.findByRole("alert")).textContent).toMatch(/unexpected upload link/);
  startAnswer = () => json({ url: `/api/import/${token}`, maxBytes: 4 });
  fireEvent.click(screen.getByRole("button", { name: "Import mail" }));
  await waitFor(() => expect(screen.getByRole("alert").textContent).toMatch(/larger than your mailbox's storage/));
  expect(FakeXHR.last).toBeNull();
});

it("explains import is for KyPost-hosted mailboxes when there are none", async () => {
  fetchMock.mockImplementation(async (url) => json(url === "/api/import" ? idle : { mailboxes: [] }));
  view();
  expect(await screen.findByText(/Import is available for mailboxes KyPost hosts/)).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Import mail" })).toBeNull();
});

it("stops polling after repeated failures and says so", async () => {
  statuses = [running];
  view({ ...user, ssoSession: true });
  await screen.findByRole("button", { name: "Cancel import" });
  fetchMock.mockImplementation(async () => json({ error: "down" }, 502));
  expect((await screen.findByRole("alert", {}, { timeout: 8000 })).textContent).toMatch(/Lost contact with the server/);
  const calls = fetchMock.mock.calls.length;
  await new Promise(r => setTimeout(r, 1500));
  expect(fetchMock.mock.calls.length).toBe(calls);
}, 12000);
