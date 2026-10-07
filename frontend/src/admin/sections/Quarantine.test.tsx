import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { AuthContext, type AuthState } from "../../auth";
import { deriveCredential } from "../../api/auth";
import { withSSOStepUp } from "../../api/stepup";
import { readQuarantine, readQuarantineChange } from "../../api/nativeMail";
import { Quarantine } from "./Quarantine";

vi.mock("../../api/auth", async (original) => ({
  ...await original<typeof import("../../api/auth")>(),
  deriveCredential: vi.fn(),
}));
vi.mock("../../api/stepup", () => ({ withSSOStepUp: vi.fn() }));

const base = "/api/admin/receiving/quarantine";
const first = { sequence: 4, gateway: "smtp-1", id: "d1/x", sender: "bounce@sender.example", receivedAt: "2026-10-07T12:00:00Z", size: 2048,
  recipients: [{ address: "sales@example.com", mailbox: "alice-id", user: "alice-id", generation: 2 }, { address: "old@example.com", mailbox: "gone-id", user: "", generation: 1 }] };
const second = { ...first, sequence: 9, id: "d2", sender: "", size: 10, recipients: [first.recipients[0]!] };
const users = { users: [{ id: "alice-id", username: "alice" }] };
let pages: Record<string, unknown>;
let answer: () => Response;
let failWrite = false, off = false;
const fetchMock = vi.fn<typeof fetch>();
const admin: AuthState = { authenticated: true, userId: "admin-id", username: "admin", role: "admin" };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const writes = () => fetchMock.mock.calls.filter(([, init]) => init?.method && init.method !== "GET");
const reads = () => fetchMock.mock.calls.filter(([url, init]) => String(url).startsWith(base) && !init?.method);
const fieldset = () => screen.getByRole("group", { name: "Confirm each action" }) as HTMLFieldSetElement;
const actions = () => screen.getByRole("group", { name: "Quarantined mail" }) as HTMLFieldSetElement;
function view(auth = admin) {
  return <AuthContext.Provider value={auth}><Quarantine /></AuthContext.Provider>;
}
async function unlocked(auth = admin) {
  render(view(auth));
  await screen.findByText("bounce@sender.example");
  await screen.findAllByText(/alice-id \/ alice$/);
  if (!auth.ssoSession) fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account-secret" } });
}
beforeEach(() => {
  pages = { [base]: { deliveries: [first, second] } };
  failWrite = false; off = false;
  answer = () => json({ gateway: "smtp-1", id: "d1/x", result: "released" });
  vi.stubGlobal("fetch", fetchMock);
  fetchMock.mockImplementation(async (url, init) => {
    if (init?.method && init.method !== "GET") {
      if (failWrite) throw new TypeError("network connection lost");
      return answer();
    }
    if (url === "/api/users") return json(users);
    if (off) return new Response("native mail is disabled", { status: 404 });
    return json(pages[String(url)] ?? { deliveries: [] });
  });
  vi.mocked(deriveCredential).mockResolvedValue({ password: "must-not-transmit", authSecret: "derived-test-secret", loginSalt: "test-salt", loginIterations: 1, derivation: "pbkdf2" });
  vi.mocked(withSSOStepUp).mockImplementation(run => run({}));
  document.cookie = "csrf_token=test-csrf";
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals(); vi.restoreAllMocks(); document.cookie = "csrf_token=; Max-Age=0"; });

it("validates untrusted quarantine pages and change answers", () => {
  expect(readQuarantine({ deliveries: [first, second] }, 0)).toHaveLength(2);
  const one = (d: unknown) => ({ deliveries: [d] });
  const r0 = first.recipients[0]!;
  for (const value of [null, {}, { deliveries: {} }, { deliveries: Array.from({ length: 101 }, (_, i) => ({ ...first, sequence: i + 1, id: `d${i}` })) },
    { deliveries: [second, first] }, { deliveries: [first, { ...first, sequence: 5 }] },
    one({ ...first, sequence: 0 }), one({ ...first, sequence: "4" }), one({ ...first, size: -1 }), one({ ...first, receivedAt: "yesterday" }),
    one({ ...first, gateway: "" }), one({ ...first, id: "a\r\nb" }), one({ ...first, id: "x".repeat(257) }), one({ ...first, sender: 7 }),
    one({ ...first, sender: "a\0@b" }), one({ ...first, sender: `${"a".repeat(320)}@b` }), one({ ...first, recipients: {} }),
    one({ ...first, recipients: [{ ...r0, address: "" }] }), one({ ...first, recipients: [{ ...r0, mailbox: "" }] }),
    one({ ...first, recipients: [{ ...r0, user: null }] }), one({ ...first, recipients: [{ ...r0, generation: -1 }] })]) {
    expect(() => readQuarantine(value, 0)).toThrow();
  }
  expect(() => readQuarantine({ deliveries: [first] }, 4)).toThrow();
  expect(readQuarantineChange({ gateway: "g", id: "i", result: "released" }, "g", "i", "release")).toBe("released");
  expect(readQuarantineChange({ gateway: "g", id: "i", result: "partially_released" }, "g", "i", "discard")).toBe("partially_released");
  for (const [value, action] of [[{ gateway: "g", id: "i", result: "discarded" }, "release"], [{ gateway: "g", id: "i", result: "released" }, "discard"],
    [{ gateway: "g", id: "other", result: "released" }, "release"], [{ gateway: "h", id: "i", result: "released" }, "release"],
    [{ gateway: "g", id: "i", result: "refused" }, "discard"], [null, "release"]] as const) {
    expect(() => readQuarantineChange(value, "g", "i", action)).toThrow();
  }
});

it("lists envelope fields as text with frozen recipients", async () => {
  render(view());
  const table = await screen.findByRole("table", { name: "Quarantined deliveries, oldest first" });
  const rows = within(table).getAllByRole("row");
  expect(rows).toHaveLength(3);
  await waitFor(() => expect(rows[1]!.textContent).toContain("sales@example.com → mailbox alice-id / alice"));
  expect(rows[1]!.textContent).toContain("bounce@sender.example");
  expect(rows[1]!.textContent).toContain("sales@example.com → mailbox alice-id / alice");
  expect(rows[1]!.textContent).toContain("old@example.com → mailbox gone-id (mailbox gone or owner changed; release will be refused)");
  expect(rows[1]!.textContent).toContain("2.0 KiB");
  const received = new Date(first.receivedAt);
  expect(within(rows[1]!).getAllByRole("cell")[0]!.textContent).toBe(received.toLocaleString(undefined, { timeZoneName: "short" }));
  expect(received.toLocaleString(undefined, { timeZoneName: "short" })).not.toBe(received.toLocaleString());
  expect(rows[1]!.textContent).toContain("smtp-1 / d1/x");
  expect(rows[2]!.textContent).toContain("(empty sender)");
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
});

it("renders a planted sender as inert text with bidi and control characters visible", async () => {
  const sender = "<img src=x onerror=alert(1)>\u202Egnp.exe@evil.example\u200B\u0007";
  pages[base] = { deliveries: [{ ...first, sender, recipients: [{ ...first.recipients[0]!, address: "a\u2066b@example.com" }] }] };
  render(view());
  const cell = await screen.findByText("<img src=x onerror=alert(1)>[U+202E]gnp.exe@evil.example[U+200B][U+0007]");
  expect(cell.tagName).toBe("TD");
  expect(cell.querySelector("img, a")).toBeNull();
  expect(document.querySelector("img")).toBeNull();
  expect(screen.getByText(/a\[U\+2066\]b@example\.com/)).toBeDefined();
  expect(document.body.textContent).not.toMatch(/[\u202E\u2066\u200B\u0007]/);
});

it("says so when there is no quarantined mail", async () => {
  pages[base] = { deliveries: [] };
  render(view());
  expect(await screen.findByText("No quarantined mail.")).toBeDefined();
  expect(screen.queryByRole("table")).toBeNull();
});

it("shows a clear state when native mail is off", async () => {
  off = true;
  render(view());
  expect(await screen.findByText("Native mail is off on this server, so there is no quarantine to review.")).toBeDefined();
  expect(screen.queryByRole("alert")).toBeNull();
  expect(screen.queryByLabelText("Account password")).toBeNull();
});

it("loads more after the last sequence", async () => {
  const full = Array.from({ length: 100 }, (_, i) => ({ ...first, sequence: i + 1, id: `d${i}` }));
  pages[base] = { deliveries: full };
  pages[`${base}?after=100`] = { deliveries: [{ ...second, sequence: 101 }] };
  render(view());
  const more = await screen.findByRole("button", { name: "Load more" });
  fireEvent.click(more);
  await screen.findByText("smtp-1 / d2");
  expect(reads().map(([url]) => url)).toEqual([base, `${base}?after=100`]);
  expect(within(screen.getByRole("table")).getAllByRole("row")).toHaveLength(102);
  expect(screen.queryByRole("button", { name: "Load more" })).toBeNull();
});

it("releases after a confirmation naming the frozen mailboxes, with credential and CSRF", async () => {
  await unlocked();
  const release = screen.getByRole("button", { name: "Release smtp-1 / d1/x" });
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(release);
  expect(confirm.mock.calls[0]?.[0]).toBe("Release only to the mailboxes this mail was frozen to when it arrived, never to an address's current owner? Each mailbox must still be active and its owner admitted. Releasing delivery d1/x from bounce@sender.example to: sales@example.com → mailbox alice-id / alice; old@example.com → mailbox gone-id (mailbox gone or owner changed; release will be refused).");
  expect(deriveCredential).not.toHaveBeenCalled();
  expect(writes()).toHaveLength(0);
  confirm.mockReturnValue(true);
  pages[base] = { deliveries: [second] };
  fireEvent.click(release);
  await screen.findByText("Released delivery d1/x from bounce@sender.example to its original mailboxes.");
  const [write] = writes();
  expect(write?.[0]).toBe(`${base}/smtp-1/d1%2Fx/release`);
  expect(write?.[1]?.method).toBe("POST");
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ authSecret: "derived-test-secret" });
  expect(write?.[1]?.credentials).toBe("include");
  expect(new Headers(write?.[1]?.headers).get("X-CSRF-Token")).toBe("test-csrf");
  expect(deriveCredential).toHaveBeenCalledWith("", "account-secret");
  expect(screen.getByLabelText("Account password").getAttribute("value")).toBe("");
  expect(screen.queryByText("smtp-1 / d1/x")).toBeNull();
});

it("discards after a confirmation that the bytes are gone for good", async () => {
  await unlocked();
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(screen.getByRole("button", { name: "Discard smtp-1 / d1/x" }));
  expect(confirm.mock.calls[0]?.[0]).toBe("Discard permanently? The bytes are deleted: the mail can never be released afterwards, and a re-pickup or receiver replay will not bring it back. If a release had started, some of its mailboxes may already hold it; those copies stay and the result is recorded as partially released. Discarding delivery d1/x from bounce@sender.example.");
  expect(writes()).toHaveLength(0);
  confirm.mockReturnValue(true);
  answer = () => json({ gateway: "smtp-1", id: "d1/x", result: "discarded" });
  fireEvent.click(screen.getByRole("button", { name: "Discard smtp-1 / d1/x" }));
  await screen.findByText("Discarded delivery d1/x from bounce@sender.example. Its bytes are deleted.");
  expect(writes()[0]?.[0]).toBe(`${base}/smtp-1/d1%2Fx/discard`);
});

it("shows a partially released discard as a warning", async () => {
  await unlocked();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ gateway: "smtp-1", id: "d1/x", result: "partially_released" });
  fireEvent.click(screen.getByRole("button", { name: "Discard smtp-1 / d1/x" }));
  const warning = await screen.findByText(/recorded as partially released: an earlier release reached some of its mailboxes, and those copies stay/);
  expect(warning.className).toContain("notice-warning");
  expect(screen.queryByText(/Its bytes are deleted\./)).toBeNull();
  expect(screen.queryByRole("alert")).toBeNull();
});

it("replays an identical request through KySignOn step-up", async () => {
  vi.mocked(withSSOStepUp).mockImplementation(async run => {
    await run({});
    return run({ "X-Kypost-Step-Up": "test-grant" });
  });
  await unlocked({ ...admin, ssoSession: true });
  expect(screen.queryByLabelText("Account password")).toBeNull();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Release smtp-1 / d1/x" }));
  await screen.findByText(/to its original mailboxes/);
  const [a, b] = writes();
  expect(writes()).toHaveLength(2);
  expect(a?.[0]).toBe(b?.[0]);
  expect(JSON.parse(String(a?.[1]?.body))).toEqual({});
  expect(new Headers(b?.[1]?.headers).get("X-Kypost-Step-Up")).toBe("test-grant");
  expect(deriveCredential).not.toHaveBeenCalled();
});

it("shows the 409 reason as returned, re-reads and restores controls", async () => {
  await unlocked();
  const before = reads().length;
  answer = () => new Response("release refused: a frozen mailbox was deleted or disabled, or its owner was offboarded, promoted or changed; discard remains available", { status: 409 });
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Release smtp-1 / d1/x" }));
  expect((await screen.findByRole("alert")).textContent).toBe("request failed: 409 - release refused: a frozen mailbox was deleted or disabled, or its owner was offboarded, promoted or changed; discard remains available");
  await waitFor(() => expect(fieldset().disabled).toBe(false));
  expect(actions().disabled).toBe(false);
  expect(reads().length).toBeGreaterThan(before);
});

it("locks until reload when no answer arrived", async () => {
  await unlocked();
  const before = reads().length;
  failWrite = true;
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Release smtp-1 / d1/x" }));
  expect((await screen.findByRole("alert")).textContent).toContain("network connection lost");
  expect(screen.getByText("Quarantine unavailable. Reload this page before making changes.")).toBeDefined();
  expect(fieldset().disabled).toBe(true);
  expect(actions().disabled).toBe(true);
  expect(reads().length).toBe(before);
});

it("locks when a success answer names another delivery", async () => {
  await unlocked();
  const before = reads().length;
  vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ gateway: "smtp-1", id: "d2", result: "released" });
  fireEvent.click(screen.getByRole("button", { name: "Release smtp-1 / d1/x" }));
  expect((await screen.findByRole("alert")).textContent).toContain("does not match the request");
  expect(fieldset().disabled).toBe(true);
  expect(reads().length).toBe(before);
});

it("says a committed change was saved when the follow-up read fails", async () => {
  await unlocked();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  pages[base] = { deliveries: "unreadable" };
  fireEvent.click(screen.getByRole("button", { name: "Release smtp-1 / d1/x" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Change saved; reload to see current quarantine. Invalid quarantine list; reload before releasing or discarding.");
  expect(fieldset().disabled).toBe(true);
});

it("offers no URL action for a dot-segment ID", async () => {
  pages[base] = { deliveries: [{ ...first, id: ".." }] };
  render(view());
  expect(await screen.findByText("Use the CLI: this ID cannot be sent in a URL.")).toBeDefined();
  expect(screen.queryByRole("button", { name: /^(Release|Discard)/ })).toBeNull();
});

it("offers no URL action for a dot-segment gateway", async () => {
  pages[base] = { deliveries: [{ ...first, gateway: "." }] };
  render(view());
  expect(await screen.findByText("Use the CLI: this ID cannot be sent in a URL.")).toBeDefined();
  expect(screen.queryByRole("button", { name: /^(Release|Discard)/ })).toBeNull();
});

it("escapes stacked combining marks after the first", async () => {
  pages[base] = { deliveries: [{ ...first, sender: "a" + "\u0336".repeat(100) + "@sender.example" }] };
  render(view());
  const cell = await screen.findByText("a\u0336[U+0336 \u00D799]@sender.example");
  expect(cell.className).toBe("quarantine-sender");
  expect(cell.closest("table")?.className).toContain("quarantine-table");
});

it("collapses a run of two but keeps single escapes and distinct neighbours apart", async () => {
  pages[base] = { deliveries: [{ ...first, sender: "x\u202E\u202E\u200By\u202E@sender.example" }] };
  render(view());
  expect(await screen.findByText("x[U+202E \u00D72][U+200B]y[U+202E]@sender.example")).toBeDefined();
});

it("shows blank letters as code points", async () => {
  const blanks = "\u034F\u115F\u1160\u17B4\u17B5\u180E\u2800\u3164\uFFA0";
  pages[base] = { deliveries: [{ ...first, sender: "ceo\u3164@corp.example", recipients: [{ ...first.recipients[0]!, address: `x${blanks}@example.com` }] }] };
  render(view());
  expect(await screen.findByText("ceo[U+3164]@corp.example")).toBeDefined();
  expect(screen.getByText(/x\[U\+034F\]\[U\+115F\]\[U\+1160\]\[U\+17B4\]\[U\+17B5\]\[U\+180E\]\[U\+2800\]\[U\+3164\]\[U\+FFA0\]@example\.com/)).toBeDefined();
});

it("stays usable when KySignOn confirmation is cancelled", async () => {
  answer = () => json({ error: "sso_step_up_required", challenge: "c1" }, 403);
  vi.mocked(withSSOStepUp).mockImplementation(async run => {
    await run({}).catch(() => undefined);
    throw new Error("confirmation cancelled");
  });
  await unlocked({ ...admin, ssoSession: true });
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Release smtp-1 / d1/x" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Not confirmed; nothing changed. confirmation cancelled");
  await waitFor(() => expect(actions().disabled).toBe(false));
  expect(screen.getByText("smtp-1 / d1/x")).toBeDefined();
});

it("stays usable when the local credential cannot be derived", async () => {
  vi.mocked(deriveCredential).mockRejectedValue(new Error("login parameters unavailable"));
  await unlocked();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Discard smtp-1 / d1/x" }));
  expect((await screen.findByRole("alert")).textContent).toBe("Not confirmed; nothing changed. login parameters unavailable");
  await waitFor(() => expect(fieldset().disabled).toBe(false));
  expect(writes()).toHaveLength(0);
});

it("keeps the change notice when native mail is off on re-read", async () => {
  await unlocked();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  off = true;
  fireEvent.click(screen.getByRole("button", { name: "Release smtp-1 / d1/x" }));
  expect(await screen.findByText("Native mail is off on this server, so there is no quarantine to review.")).toBeDefined();
  expect(screen.getByText("Released delivery d1/x from bounce@sender.example to its original mailboxes.")).toBeDefined();
});

const unresolved = { ...first, sequence: 12, gateway: "cloudflare-continuous", id: "u1", sender: "late@sender.example", unresolved: true,
  recipients: [{ address: "sales@example.com", mailbox: "", user: "", generation: 1, currentMailbox: "alice-id", currentUser: "alice-id" }] };

it("validates unresolved recipients", () => {
  expect(readQuarantine({ deliveries: [unresolved] }, 0)[0]?.unresolved).toBe(true);
  const r0 = unresolved.recipients[0]!;
  for (const d of [{ ...unresolved, unresolved: "yes" }, { ...unresolved, recipients: [{ ...r0, mailbox: "alice-id" }] }, { ...unresolved, recipients: [{ ...r0, user: "alice-id" }] },
    { ...first, recipients: [{ ...first.recipients[0]!, currentMailbox: "x" }] }, { ...unresolved, recipients: [{ ...r0, currentUser: "a\nb" }] }]) {
    expect(() => readQuarantine({ deliveries: [d] }, 0)).toThrow();
  }
});

it("releases unresolved mail only to the current owner after saying the owner is not proven", async () => {
  pages[base] = { deliveries: [first, unresolved] };
  answer = () => json({ gateway: "cloudflare-continuous", id: "u1", result: "released" });
  await unlocked();
  expect(screen.queryByRole("button", { name: "Release cloudflare-continuous / u1" })).toBeNull();
  expect(screen.getByRole("note").textContent).toContain("routing table this server does not know");
  const release = screen.getByRole("button", { name: "Release cloudflare-continuous / u1 to the current owner" });
  expect(release.textContent).toBe("Release to current owner…");
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(release);
  expect(confirm.mock.calls[0]?.[0]).toBe("This mail was captured under a routing table this server does not know (for example mail that waited at Cloudflare through a restore), so its original owner cannot be proven. Release it to the address's current owner instead? The owner shown is today's, not proven to be the one it was sent to. Releasing delivery u1 from late@sender.example to: sales@example.com → original owner unknown; today's owner: mailbox alice-id / alice.");
  expect(writes()).toHaveLength(0);
  confirm.mockReturnValue(true);
  pages[base] = { deliveries: [first] };
  fireEvent.click(release);
  await screen.findByText("Released delivery u1 from late@sender.example to the address's current owner.");
  const [write] = writes();
  expect(write?.[0]).toBe(`${base}/cloudflare-continuous/u1/release`);
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ authSecret: "derived-test-secret", toCurrentOwner: true, currentMailbox: "alice-id" });
});

it("says release will be refused when the unresolved address is inactive", async () => {
  pages[base] = { deliveries: [{ ...unresolved, recipients: [{ ...unresolved.recipients[0]!, currentMailbox: "", currentUser: "" }] }] };
  render(view());
  await screen.findByText(/original owner unknown; the address is not active now, so release will be refused/);
  expect((screen.getByRole("button", { name: "Release cloudflare-continuous / u1 to the current owner" }) as HTMLButtonElement).disabled).toBe(true);
});
