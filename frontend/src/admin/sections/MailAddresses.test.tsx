import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { AuthContext, type AuthState } from "../../auth";
import { deriveCredential } from "../../api/auth";
import { withSSOStepUp } from "../../api/stepup";
import { readMailAddressChange, readMailAddresses } from "../../api/nativeMail";
import { MailAddresses } from "./MailAddresses";

vi.mock("../../api/auth", async (original) => ({
  ...await original<typeof import("../../api/auth")>(),
  deriveCredential: vi.fn(),
}));
vi.mock("../../api/stepup", () => ({ withSSOStepUp: vi.fn() }));

const sales = { address: "sales@example.com", mailbox: "alice-id", kind: "alias", state: "active", generation: 1 };
const old = { address: "old@example.com", mailbox: "alice-id", kind: "alias", state: "reserved", generation: 2 };
const list = { mailboxes: [
  { mailbox: "alice-id", user: "alice-id", kind: "primary", addresses: [
    { address: "alice@example.com", mailbox: "alice-id", kind: "primary", state: "active", generation: 3 }, old, sales,
  ] },
  { mailbox: "bob-id", user: "bob-id", kind: "primary", addresses: [
    { address: "bob@example.com", mailbox: "bob-id", kind: "primary", state: "disabled", generation: 1 },
  ] },
] };
const users = { users: [{ id: "alice-id", username: "alice" }, { id: "bob-id", username: "bob" }] };
let listResponse: unknown;
let answer: () => Response;
let failWrite = false, failUsers = false;
const fetchMock = vi.fn<typeof fetch>();
const admin: AuthState = { authenticated: true, userId: "admin-id", username: "admin", role: "admin" };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const writes = () => fetchMock.mock.calls.filter(([, init]) => init?.method && init.method !== "GET");
const reads = () => fetchMock.mock.calls.filter(([url, init]) => url === "/api/admin/mail-addresses" && !init?.method);
const fieldset = () => screen.getByRole("group", { name: "Confirm each action" }) as HTMLFieldSetElement;
const actions = () => screen.getByRole("group", { name: "Mailboxes and aliases" }) as HTMLFieldSetElement;
function view(auth = admin) {
  return <AuthContext.Provider value={auth}><MailAddresses /></AuthContext.Provider>;
}
async function unlocked(auth = admin) {
  render(view(auth));
  await screen.findByText("sales@example.com");
  if (!auth.ssoSession) fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account-secret" } });
}
beforeEach(() => {
  listResponse = list; failWrite = false; failUsers = false;
  answer = () => json({ ...sales, address: "new@example.com" });
  vi.stubGlobal("fetch", fetchMock);
  fetchMock.mockImplementation(async (url, init) => {
    if (init?.method && init.method !== "GET") {
      // A rejected fetch is an answer that never arrived.
      if (failWrite) throw new TypeError("network connection lost");
      return answer();
    }
    if (url === "/api/users" && failUsers) return new Response("users unreadable", { status: 503 });
    return json(url === "/api/users" ? users : listResponse);
  });
  vi.mocked(deriveCredential).mockResolvedValue({ password: "must-not-transmit", authSecret: "derived-test-secret", loginSalt: "test-salt", loginIterations: 1, derivation: "pbkdf2" });
  vi.mocked(withSSOStepUp).mockImplementation(run => run({}));
  document.cookie = "csrf_token=test-csrf";
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals(); vi.restoreAllMocks(); document.cookie = "csrf_token=; Max-Age=0"; });

it("validates untrusted address lists and change answers", () => {
  expect(readMailAddresses(list)[0]?.addresses).toHaveLength(3);
  const box = list.mailboxes[0]!;
  const with1 = (address: unknown) => ({ mailboxes: [{ ...box, addresses: [address] }] });
  for (const value of [null, {}, { mailboxes: {} }, { mailboxes: [{ ...box, mailbox: "" }] },
    with1({ ...sales, state: "deleted" }), with1({ ...sales, kind: "forward" }), with1({ ...sales, generation: 0 }),
    with1({ ...sales, generation: "1" }), with1({ ...sales, address: "Sales@example.com" }), with1({ ...sales, address: "<b>x</b>@example.com" }),
    with1({ ...sales, address: `${"a".repeat(65)}@example.com` }), with1({ ...sales, mailbox: "bob-id" }), with1({ ...sales, address: "x".repeat(2000) }),
    { mailboxes: [box, { ...box, mailbox: "bob-id", addresses: [{ ...sales, mailbox: "bob-id" }] }] }]) {
    expect(() => readMailAddresses(value)).toThrow();
  }
  expect(readMailAddressChange({ ...sales, warning: "routes pending" }, "Sales@Example.com").warning).toBe("routes pending");
  expect(() => readMailAddressChange(sales, "other@example.com")).toThrow();
  expect(() => readMailAddressChange({ ...sales, warning: 7 }, "sales@example.com")).toThrow();
});

it("lists addresses as text by state, filters by user and offers no primary actions", async () => {
  render(view());
  const alice = await screen.findByRole("table", { name: "alice@example.com (alice)" });
  const rows = within(alice).getAllByRole("row");
  expect(rows.map(r => r.textContent)).toEqual([
    "AddressKindStateGenerationActions",
    "alice@example.comprimaryactive3",
    expect.stringMatching(/^old@example\.comaliasreserved2Reassign/),
    "sales@example.comaliasactive1Release",
  ]);
  expect(within(rows[1]!).queryByRole("button")).toBeNull();
  expect(screen.getByText("disabled")).toBeDefined();
  expect(screen.queryByRole("button", { name: "Release alice@example.com" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Reassign alice@example.com" })).toBeNull();
  fireEvent.change(screen.getByLabelText("Show mailboxes of"), { target: { value: "bob-id" } });
  expect(screen.queryByText("sales@example.com")).toBeNull();
  expect(screen.getByText("bob@example.com")).toBeDefined();
});

it("refuses malformed server JSON and keeps changes disabled", async () => {
  listResponse = { mailboxes: [{ ...list.mailboxes[0], addresses: [{ ...sales, state: "gone" }] }] };
  render(view());
  expect((await screen.findByRole("alert")).textContent).toContain("Invalid mail address record");
  expect(fieldset().disabled).toBe(true);
});

it("adds an alias with the derived credential and CSRF client", async () => {
  await unlocked();
  const add = screen.getByRole("button", { name: "Add alias" });
  expect(add.hasAttribute("disabled")).toBe(true);
  fireEvent.change(screen.getByLabelText("Mailbox"), { target: { value: "alice-id" } });
  fireEvent.change(screen.getByLabelText("Alias address"), { target: { value: " new@example.com " } });
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(add);
  expect(confirm.mock.calls[0]?.[0]).toBe("Add new@example.com to alice@example.com (alice)? KyPost holds this address permanently: records are never deleted, and releasing it later only reserves it.");
  expect(deriveCredential).not.toHaveBeenCalled();
  expect(writes()).toHaveLength(0);
  confirm.mockReturnValue(true);
  fireEvent.click(add);
  await screen.findByText("new@example.com is active on alice@example.com (alice) (generation 1).");
  const [write] = writes();
  expect(write?.[0]).toBe("/api/admin/mail-addresses");
  expect(write?.[1]?.method).toBe("POST");
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ mailbox: "alice-id", address: "new@example.com", authSecret: "derived-test-secret" });
  expect(write?.[1]?.credentials).toBe("include");
  expect(new Headers(write?.[1]?.headers).get("X-CSRF-Token")).toBe("test-csrf");
  expect(deriveCredential).toHaveBeenCalledWith("", "account-secret");
  expect(screen.getByLabelText("Account password").getAttribute("value")).toBe("");
  expect(screen.getByLabelText("Alias address").getAttribute("value")).toBe("");
});

it("releases only after confirmation that names the consequences", async () => {
  await unlocked();
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(screen.getByRole("button", { name: "Release sales@example.com" }));
  expect(confirm.mock.calls[0]?.[0]).toMatch(/reserved.*stop immediately.*stays in alice@example\.com.*explicitly reassigns/);
  expect(deriveCredential).not.toHaveBeenCalled();
  expect(writes()).toHaveLength(0);
  confirm.mockReturnValue(true);
  answer = () => json({ ...sales, state: "reserved", generation: 2 });
  fireEvent.click(screen.getByRole("button", { name: "Release sales@example.com" }));
  await screen.findByText(/sales@example\.com released and reserved \(generation 2\)/);
  const [write] = writes();
  expect(write?.[0]).toBe("/api/admin/mail-addresses/sales%40example.com");
  expect(write?.[1]?.method).toBe("DELETE");
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ authSecret: "derived-test-secret" });
  expect(new Headers(write?.[1]?.headers).get("X-CSRF-Token")).toBe("test-csrf");
});

it("reassigns a reserved alias to the confirmed target mailbox", async () => {
  await unlocked();
  fireEvent.change(screen.getByLabelText("Reassign old@example.com to"), { target: { value: "bob-id" } });
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(screen.getByRole("button", { name: "Reassign old@example.com" }));
  expect(confirm.mock.calls[0]?.[0]).toContain("Reassign old@example.com to bob@example.com (bob)?");
  expect(writes()).toHaveLength(0);
  confirm.mockReturnValue(true);
  answer = () => json({ ...old, mailbox: "bob-id", state: "active", generation: 3 });
  fireEvent.click(screen.getByRole("button", { name: "Reassign old@example.com" }));
  await screen.findByText("old@example.com is active on bob@example.com (bob) (generation 3).");
  const [write] = writes();
  expect(write?.[0]).toBe("/api/admin/mail-addresses/old%40example.com/reassign");
  expect(write?.[1]?.method).toBe("POST");
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ mailbox: "bob-id", authSecret: "derived-test-secret" });
});

it("replays an identical request through KySignOn step-up", async () => {
  vi.mocked(withSSOStepUp).mockImplementation(async run => {
    await run({});
    return run({ "X-Kypost-Step-Up": "test-grant" });
  });
  await unlocked({ ...admin, ssoSession: true });
  expect(screen.queryByLabelText("Account password")).toBeNull();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ ...sales, state: "reserved", generation: 2 });
  fireEvent.click(screen.getByRole("button", { name: "Release sales@example.com" }));
  await screen.findByText(/released and reserved/);
  const [first, second] = writes();
  expect(writes()).toHaveLength(2);
  expect(first?.[0]).toBe(second?.[0]);
  expect(first?.[1]?.body).toBe(second?.[1]?.body);
  expect(JSON.parse(String(first?.[1]?.body))).toEqual({});
  expect(new Headers(second?.[1]?.headers).get("X-Kypost-Step-Up")).toBe("test-grant");
  expect(deriveCredential).not.toHaveBeenCalled();
});

it("shows an answered refusal as returned, re-reads and restores controls", async () => {
  await unlocked();
  const before = reads().length;
  answer = () => new Response("administrator identities own no aliases; use the everyday identity's mailbox", { status: 409 });
  fireEvent.change(screen.getByLabelText("Mailbox"), { target: { value: "alice-id" } });
  fireEvent.change(screen.getByLabelText("Alias address"), { target: { value: "new@example.com" } });
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Add alias" }));
  expect((await screen.findByRole("alert")).textContent).toBe("request failed: 409 - administrator identities own no aliases; use the everyday identity's mailbox");
  await waitFor(() => expect(fieldset().disabled).toBe(false));
  expect(actions().disabled).toBe(false);
  expect(reads().length).toBeGreaterThan(before);
  expect(screen.getByText("sales@example.com")).toBeDefined();
});

it("shows a committed change with pending routes as a warning, not an error", async () => {
  await unlocked();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ ...sales, state: "reserved", generation: 2, warning: "address change saved, but its receiving route is not yet inactive" });
  fireEvent.click(screen.getByRole("button", { name: "Release sales@example.com" }));
  const warning = await screen.findByText(/Change saved with a warning: address change saved/);
  expect(warning.className).toContain("notice-warning");
  expect(screen.getByText(/released and reserved/)).toBeDefined();
  expect(screen.queryByRole("alert")).toBeNull();
  expect(fieldset().disabled).toBe(false);
});

it("locks changes until reload when no answer arrived", async () => {
  await unlocked();
  const before = reads().length;
  failWrite = true;
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Release sales@example.com" }));
  expect((await screen.findByRole("alert")).textContent).toContain("network connection lost");
  expect(screen.getByText("Address list unavailable. Reload this page before making changes.")).toBeDefined();
  expect(fieldset().disabled).toBe(true);
  expect(actions().disabled).toBe(true);
  expect(reads().length).toBe(before);
});

it("locks changes when a success answer does not match the request", async () => {
  await unlocked();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ ...sales, state: "active" });
  fireEvent.click(screen.getByRole("button", { name: "Release sales@example.com" }));
  expect((await screen.findByRole("alert")).textContent).toContain("does not match the request");
  expect(fieldset().disabled).toBe(true);
});

it("keeps working with IDs when user names cannot be read", async () => {
  failUsers = true;
  render(view());
  expect(await screen.findByRole("table", { name: "alice@example.com (alice-id)" })).toBeDefined();
  expect(screen.queryByRole("alert")).toBeNull();
  expect(actions().disabled).toBe(false);
});

it("falls back to all users when the filtered owner disappears", async () => {
  await unlocked();
  fireEvent.change(screen.getByLabelText("Show mailboxes of"), { target: { value: "bob-id" } });
  expect(screen.queryByText("sales@example.com")).toBeNull();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  listResponse = { mailboxes: [list.mailboxes[0]] };
  fireEvent.change(screen.getByLabelText("Mailbox"), { target: { value: "alice-id" } });
  fireEvent.change(screen.getByLabelText("Alias address"), { target: { value: "new@example.com" } });
  fireEvent.click(screen.getByRole("button", { name: "Add alias" }));
  await screen.findByText(/new@example\.com is active/);
  expect((screen.getByLabelText("Show mailboxes of") as HTMLSelectElement).value).toBe("");
  expect(screen.getByText("sales@example.com")).toBeDefined();
});

it("says a committed change was saved when the follow-up read fails", async () => {
  await unlocked();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ ...sales, state: "reserved", generation: 2 });
  listResponse = { mailboxes: "unreadable" };
  fireEvent.click(screen.getByRole("button", { name: "Release sales@example.com" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Change saved; reload to see current addresses. Invalid mailbox list.");
  expect(fieldset().disabled).toBe(true);
});
