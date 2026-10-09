import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { AuthContext, type AuthState } from "../../auth";
import { deriveCredential } from "../../api/auth";
import { withSSOStepUp } from "../../api/stepup";
import { readSenderBlockAdded, readSenderBlockRemoved, readSenderBlocks } from "../../api/nativeMail";
import { expiryMs, SenderBlocks } from "./SenderBlocks";

vi.mock("../../api/auth", async (original) => ({
  ...await original<typeof import("../../api/auth")>(),
  deriveCredential: vi.fn(),
}));
vi.mock("../../api/stepup", () => ({ withSSOStepUp: vi.fn() }));

const base = "/api/admin/receiving/blocks";
const manual = { id: "0123456789abcdef", kind: "address", value: "spammer@bad.example", until: null, source: "manual", level: 0, createdAt: Date.UTC(2026, 9, 1), actor: "admin-id", reason: "spam" };
const auto = { id: "fedcba9876543210", kind: "domain", value: "provider.example", until: Date.now() + 86_400_000, source: "automatic", level: 2, createdAt: Date.UTC(2026, 9, 6), actor: "automatic", reason: "abuse" };
const quiet = { damaged: false, resetAt: null, domainBlocksFrom: Date.UTC(2026, 0, 1), goodFull: false, automaticFull: false };
let list: unknown;
let answer: () => Response;
let failWrite = false, off = false;
const fetchMock = vi.fn<typeof fetch>();
const admin: AuthState = { authenticated: true, userId: "admin-id", username: "admin", role: "admin" };
const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const writes = () => fetchMock.mock.calls.filter(([, init]) => init?.method && init.method !== "GET");
const reads = () => fetchMock.mock.calls.filter(([, init]) => !init?.method);
const fieldset = () => screen.getByRole("group", { name: "Confirm each action" }) as HTMLFieldSetElement;
const table = () => screen.getByRole("group", { name: "Blocks in force" }) as HTMLFieldSetElement;
const view = (auth = admin) => <AuthContext.Provider value={auth}><SenderBlocks /></AuthContext.Provider>;
async function unlocked(auth = admin) {
  render(view(auth));
  await screen.findByText("spammer@bad.example");
  if (!auth.ssoSession) fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account-secret" } });
}
function fill(kind: string, value: string, expiry = "", reason = "spam") {
  fireEvent.change(screen.getByLabelText("Kind"), { target: { value: kind } });
  fireEvent.change(screen.getByLabelText(`Sender ${kind}`), { target: { value } });
  fireEvent.change(screen.getByLabelText("Expires (optional; empty means no expiry)"), { target: { value: expiry } });
  fireEvent.change(screen.getByLabelText("Reason"), { target: { value: reason } });
}
const at = (ms: number) => new Date(ms).toLocaleString(undefined, { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" });
const removeManual = () => screen.getByRole("button", { name: "Remove address block spammer@bad.example" });
beforeEach(() => {
  list = { blocks: [manual, auto], evidence: quiet };
  failWrite = false; off = false;
  answer = () => json({ id: manual.id, result: "unblocked" });
  vi.stubGlobal("fetch", fetchMock);
  fetchMock.mockImplementation(async (_url, init) => {
    if (init?.method && init.method !== "GET") {
      if (failWrite) throw new TypeError("network connection lost");
      return answer();
    }
    if (off) return new Response("native mail is disabled", { status: 404 });
    return json(list);
  });
  vi.mocked(deriveCredential).mockResolvedValue({ password: "must-not-transmit", authSecret: "derived-test-secret", loginSalt: "test-salt", loginIterations: 1, derivation: "pbkdf2" });
  vi.mocked(withSSOStepUp).mockImplementation(run => run({}));
  document.cookie = "csrf_token=test-csrf";
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals(); vi.restoreAllMocks(); document.cookie = "csrf_token=; Max-Age=0"; });

it("validates untrusted block lists and change answers", () => {
  expect(readSenderBlocks({ blocks: [manual, auto], evidence: quiet, future: 1 }).blocks).toHaveLength(2);
  expect(readSenderBlocks({ blocks: [{ ...auto, level: 7 }], evidence: quiet }).blocks[0]?.level).toBe(7);
  expect(readSenderBlocks({ blocks: Array.from({ length: 5000 }, (_, i) => ({ ...manual, id: i.toString(16).padStart(16, "0") })), evidence: quiet }).blocks).toHaveLength(5000);
  expect(readSenderBlocks({ blocks: [{ ...manual, extra: "x" }], evidence: { ...quiet, extra: true } }).blocks[0]?.value).toBe(manual.value);
  const one = (b: unknown) => ({ blocks: [b], evidence: quiet });
  for (const value of [null, {}, { blocks: [] }, { blocks: {}, evidence: quiet }, { blocks: [manual, manual], evidence: quiet },
    { blocks: Array.from({ length: 5001 }, (_, i) => ({ ...manual, id: i.toString(16).padStart(16, "0") })), evidence: quiet },
    { blocks: [], evidence: { ...quiet, damaged: "no" } }, { blocks: [], evidence: { ...quiet, goodFull: null } }, { blocks: [], evidence: { ...quiet, automaticFull: 1 } },
    { blocks: [], evidence: { ...quiet, resetAt: "2026-10-01" } }, { blocks: [], evidence: { ...quiet, domainBlocksFrom: undefined } },
    one({ ...manual, id: "0123" }), one({ ...manual, id: "0123456789ABCDEF" }), one({ ...manual, kind: "ip" }), one({ ...auto, kind: "ip" }), one({ ...manual, value: "" }),
    one({ ...manual, value: "a\r\n@b.example" }), one({ ...manual, value: `${"a".repeat(320)}@b` }), one({ ...manual, value: "bad.example" }), one({ ...auto, value: "a@provider.example" }),
    one({ ...manual, source: "imported" }), one({ ...manual, level: 1 }), one({ ...auto, level: 0 }), one({ ...auto, level: 1.5 }), one({ ...auto, level: "2" }),
    one({ ...manual, until: "never" }), one({ ...manual, until: 1.5 }), one({ ...manual, until: -1 }), one({ ...manual, until: undefined }), one({ ...manual, createdAt: null }),
    one({ ...manual, actor: "" }), one({ ...manual, reason: "spite" })]) {
    expect(() => readSenderBlocks(value)).toThrow();
  }
  const want = { kind: "address", value: "Spammer@Bad.example", until: null, reason: "spam" } as const;
  expect(readSenderBlockAdded({ block: manual }, want).id).toBe(manual.id);
  for (const value of [{}, { block: { ...manual, value: "other@bad.example" } }, { block: { ...manual, kind: "domain", value: "bad.example" } },
    { block: { ...manual, until: 5 } }, { block: { ...manual, reason: "other" } }, { block: { ...manual, source: "automatic", level: 1 } }, manual]) {
    expect(() => readSenderBlockAdded(value, want)).toThrow();
  }
  expect(readSenderBlockRemoved({ id: "x", result: "unblocked" }, "x")).toBe("");
  expect(readSenderBlockRemoved({ id: "x", result: "unblocked", warning: "w" }, "x")).toBe("w");
  for (const value of [{ id: "y", result: "unblocked" }, { id: "x", result: "refused" }, { id: "x", result: "unblocked", warning: 7 }, null]) {
    expect(() => readSenderBlockRemoved(value, "x")).toThrow();
  }
});

it("converts the expiry input as local time and refuses garbage", () => {
  const now = new Date(2026, 9, 7, 9, 0).getTime();
  expect(expiryMs("", now)).toBeNull();
  expect(expiryMs("2026-10-08T12:30", now)).toBe(new Date(2026, 9, 8, 12, 30).getTime());
  expect(expiryMs("2026-10-08T12:30:15", now)).toBe(new Date(2026, 9, 8, 12, 30, 15).getTime());
  expect(expiryMs("2026-10-07T09:01", now)).toBe(now + 60_000);
  for (const bad of ["tomorrow", "2026-10-08", "2026-10-08T25:00", "2026-10-08T12:30Z", "1760000000000"]) expect(() => expiryMs(bad, now)).toThrow("not a valid");
  for (const past of ["2026-10-07T09:00", "2026-10-06T23:59"]) expect(() => expiryMs(past, now)).toThrow("Expiry must be in the future.");
});

it("lists blocks as text and marks automatic domain blocks in words", async () => {
  render(view());
  const rows = within(await screen.findByRole("table", { name: "Sender blocks in force" })).getAllByRole("row");
  expect(rows).toHaveLength(3);
  const cells = (i: number) => within(rows[i]!).getAllByRole("cell").map(c => c.textContent);
  expect(cells(1)).toEqual(["address", "spammer@bad.example", "manual", "—", "no expiry", at(manual.createdAt), "spam", "Remove…"]);
  expect(cells(2)).toEqual(["domain", "provider.example", "automatic domain", "2", at(auto.until), at(auto.createdAt), "abuse", "Remove…"]);
  expect(within(rows[2]!).getByText("automatic domain").tagName).toBe("STRONG");
  expect(within(rows[1]!).getAllByRole("cell")[1]!.className).toBe("sender-block-value");
  expect(screen.getByText("1 automatic domain block refuses every sender at its domain, which can be a whole mail provider. Remove it if your users get mail from there.")).toBeDefined();
  expect(screen.queryAllByRole("note")).toHaveLength(1);
});

it("renders hostile values as inert text with bidi and control characters visible", async () => {
  const value = "<img src=x onerror=alert(1)>\u202Egnp.exe@evil.example\u200B\u0007";
  list = { blocks: [{ ...manual, value }], evidence: quiet };
  render(view());
  const cell = await screen.findByText("<img src=x onerror=alert(1)>[U+202E]gnp.exe@evil.example[U+200B][U+0007]");
  expect(cell.tagName).toBe("TD");
  expect(document.querySelector("img")).toBeNull();
  expect(screen.getByRole("button", { name: "Remove address block <img src=x onerror=alert(1)>[U+202E]gnp.exe@evil.example[U+200B][U+0007]" })).toBeDefined();
  expect(document.body.innerHTML).not.toMatch(/[\u202E\u200B\u0007]/);
  fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account-secret" } });
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(screen.getByRole("button", { name: /^Remove address block/ }));
  fill("address", "ceo\u202E\u3164@corp.example");
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect(confirm.mock.calls[0]?.[0]).toContain("block on <img src=x onerror=alert(1)>[U+202E]gnp.exe@evil.example[U+200B][U+0007]?");
  expect(confirm.mock.calls[1]?.[0]).toContain("Block the address ceo[U+202E][U+3164]@corp.example with no expiry");
  expect(confirm.mock.calls.join("")).not.toMatch(/[\u202E\u200B\u0007\u3164]/);
  expect(writes()).toHaveLength(0);
});

it("shows only the evidence states that apply", async () => {
  list = { blocks: [manual], evidence: quiet };
  render(view());
  await screen.findByText("spammer@bad.example");
  expect(screen.queryAllByRole("note")).toHaveLength(0);
  cleanup();
  list = { blocks: [manual], evidence: { damaged: true, resetAt: null, domainBlocksFrom: null, goodFull: false, automaticFull: false } };
  render(view());
  await screen.findByText("spammer@bad.example");
  expect(screen.getAllByRole("note").map(n => n.textContent)).toEqual([
    "Automatic-block evidence is unreadable. A malformed file is set aside at the next write and counting restarts; any other read failure needs storage repair. Blocks in force are unaffected.",
  ]);
  cleanup();
  list = { blocks: [manual], evidence: { ...quiet, domainBlocksFrom: null } };
  render(view());
  await screen.findByText("spammer@bad.example");
  expect(screen.getAllByRole("note").map(n => n.textContent)).toEqual([
    "Automatic blocks (address and domain) need Maddy with the Rspamd sidecar. Automatic domain blocks are not active yet: they start 30 days after the first authenticated mail is recorded.",
  ]);
  cleanup();
  const reset = Date.UTC(2026, 9, 2), later = Date.now() + 86_400_000;
  list = { blocks: [manual], evidence: { damaged: false, resetAt: reset, domainBlocksFrom: later, goodFull: true, automaticFull: true } };
  render(view());
  await screen.findByText("spammer@bad.example");
  expect(screen.getAllByRole("note").map(n => n.textContent)).toEqual([
    `Automatic-block evidence was damaged and restarted at ${at(reset)}; counts before then are lost.`,
    `Automatic domain blocks are not active until ${at(later)}.`,
    "Automatic blocking is full: new automatic blocks are refused until some expire. Add a manual domain block to cover a flood.",
    "New domains are no longer protected from automatic domain blocks: the record of domains that sent authenticated mail is full.",
  ]);
});

it("shows a clear state when native mail is off", async () => {
  off = true;
  render(view());
  expect(await screen.findByText("Native mail is off on this server, so there are no sender blocks to manage.")).toBeDefined();
  expect(screen.queryByLabelText("Account password")).toBeNull();
});

it("adds a block after a confirmation naming the value, with credential and CSRF", async () => {
  await unlocked();
  fill("domain", " Bad.Example ", "2099-10-08T12:30", "phishing");
  const until = new Date(2099, 9, 8, 12, 30).getTime();
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect(confirm.mock.calls[0]?.[0]).toBe(`Block the domain Bad.Example until ${new Date(until).toLocaleString(undefined, { year: "numeric", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" })} (reason: phishing)? Its mail is refused at reception from now on; mail already received is unaffected. Only this exact domain is blocked, not its subdomains. If the list is full, the automatic blocks that expire soonest are removed to make room. This deployment's own mail domains, and addresses on them, cannot be blocked.`);
  expect(deriveCredential).not.toHaveBeenCalled();
  expect(writes()).toHaveLength(0);
  confirm.mockReturnValue(true);
  const added = { ...manual, id: "1111111111111111", kind: "domain", value: "bad.example", until, reason: "phishing" };
  answer = () => json({ block: added });
  list = { blocks: [manual, auto, added], evidence: quiet };
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  await screen.findByText("Blocked the domain Bad.Example.");
  const [write] = writes();
  expect(write?.[0]).toBe(base);
  expect(write?.[1]?.method).toBe("POST");
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ authSecret: "derived-test-secret", kind: "domain", value: "Bad.Example", until, reason: "phishing" });
  expect(new Headers(write?.[1]?.headers).get("X-CSRF-Token")).toBe("test-csrf");
  expect(deriveCredential).toHaveBeenCalledWith("", "account-secret");
  expect(screen.getByText("bad.example")).toBeDefined();
  expect((screen.getByLabelText("Sender domain") as HTMLInputElement).value).toBe("");
});

it("sends no until for an empty expiry", async () => {
  await unlocked();
  fill("address", "x@bad.example");
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ block: { ...manual, id: "2222222222222222", value: "x@bad.example" } });
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  await screen.findByText("Blocked the address x@bad.example.");
  expect(confirm.mock.calls[0]?.[0]).toContain("x@bad.example with no expiry (reason: spam)?");
  expect(JSON.parse(String(writes()[0]?.[1]?.body))).toEqual({ authSecret: "derived-test-secret", kind: "address", value: "x@bad.example", reason: "spam" });
});

it("keeps a leading non-ASCII space in an address and strips only ASCII padding", async () => {
  await unlocked();
  const sender = String.fromCodePoint(0x3000) + "x@bad.example";
  fill("address", "  " + sender + " ");
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ block: { ...manual, id: "3333333333333333", value: sender } });
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  await screen.findByText("Blocked the address [U+3000]x@bad.example.");
  expect(confirm.mock.calls[0]?.[0]).toContain("address [U+3000]x@bad.example with no expiry");
  expect(JSON.parse(String(writes()[0]?.[1]?.body)).value).toBe(sender);
});

it("removes a block after saying automatic re-blocking is suppressed", async () => {
  await unlocked();
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(removeManual());
  expect(confirm.mock.calls[0]?.[0]).toBe("Remove the manual address block on spammer@bad.example? Its mail is accepted again from now on. Automatic blocks of this exact address are then suppressed for 30 days.");
  expect(writes()).toHaveLength(0);
  confirm.mockReturnValue(true);
  list = { blocks: [auto], evidence: quiet };
  fireEvent.click(removeManual());
  await screen.findByText("Removed the manual address block on spammer@bad.example.");
  const [write] = writes();
  expect(write?.[0]).toBe(`${base}/${manual.id}`);
  expect(write?.[1]?.method).toBe("DELETE");
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ authSecret: "derived-test-secret" });
  expect(new Headers(write?.[1]?.headers).get("X-CSRF-Token")).toBe("test-csrf");
  expect(screen.queryByText("spammer@bad.example")).toBeNull();
});

it("shows the removal warning beside the notice", async () => {
  await unlocked();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ id: manual.id, result: "unblocked", warning: "block removed, but automatic re-blocking could not be suppressed, so it may come back; check receiving storage" });
  fireEvent.click(removeManual());
  const warning = await screen.findByText("block removed, but automatic re-blocking could not be suppressed, so it may come back; check receiving storage");
  expect(warning.className).toContain("notice-warning");
  expect(screen.getByText("Removed the manual address block on spammer@bad.example.")).toBeDefined();
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
  fireEvent.click(removeManual());
  await screen.findByText(/^Removed the manual/);
  const [a, b] = writes();
  expect(writes()).toHaveLength(2);
  expect(JSON.parse(String(a?.[1]?.body))).toEqual({});
  expect(new Headers(b?.[1]?.headers).get("X-Kypost-Step-Up")).toBe("test-grant");
  expect(deriveCredential).not.toHaveBeenCalled();
});

it("shows the 409 reason as returned, re-reads and restores controls", async () => {
  await unlocked();
  fill("domain", "example.com");
  const before = reads().length;
  answer = () => new Response("refused: that is one of this deployment's own mail domains; blocking it would refuse your own users' mail", { status: 409 });
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect((await screen.findByRole("alert")).textContent).toBe("request failed: 409 - refused: that is one of this deployment's own mail domains; blocking it would refuse your own users' mail");
  await waitFor(() => expect(table().disabled).toBe(false));
  expect(fieldset().disabled).toBe(false);
  expect(reads().length).toBeGreaterThan(before);
});

it("shows a wrong step-up password and stays usable without reloading", async () => {
  const reload = vi.fn();
  vi.stubGlobal("location", { ...window.location, reload });
  await unlocked();
  answer = () => new Response("invalid credentials\n", { status: 403 });
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(removeManual());
  expect((await screen.findByRole("alert")).textContent).toBe("request failed: 403 - invalid credentials");
  await waitFor(() => expect(table().disabled).toBe(false));
  expect(fieldset().disabled).toBe(false);
  expect(screen.getByText("spammer@bad.example")).toBeTruthy();
  expect(reload).not.toHaveBeenCalled();
});

it("locks until reload when no answer arrived", async () => {
  await unlocked();
  const before = reads().length;
  failWrite = true;
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(removeManual());
  expect((await screen.findByRole("alert")).textContent).toContain("network connection lost");
  expect(screen.getByText("Sender blocks unavailable. Reload this page before making changes.")).toBeDefined();
  expect(fieldset().disabled).toBe(true);
  expect(table().disabled).toBe(true);
  expect(reads().length).toBe(before);
});

it("locks when a success answer names another block", async () => {
  await unlocked();
  const before = reads().length;
  vi.spyOn(window, "confirm").mockReturnValue(true);
  answer = () => json({ id: auto.id, result: "unblocked" });
  fireEvent.click(removeManual());
  expect((await screen.findByRole("alert")).textContent).toContain("does not match the request");
  expect(fieldset().disabled).toBe(true);
  expect(reads().length).toBe(before);
});

it("stays usable when KySignOn confirmation is cancelled", async () => {
  answer = () => json({ error: "sso_step_up_required", challenge: "c1" }, 403);
  vi.mocked(withSSOStepUp).mockImplementation(async run => {
    await run({}).catch(() => undefined);
    throw new Error("confirmation cancelled");
  });
  await unlocked({ ...admin, ssoSession: true });
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(removeManual());
  expect((await screen.findByRole("alert")).textContent).toBe("Not confirmed; nothing changed. confirmation cancelled");
  await waitFor(() => expect(table().disabled).toBe(false));
  expect(screen.getByText("spammer@bad.example")).toBeDefined();
});

const autoAddress = (n: number, until: number) => ({ ...auto, id: n.toString(16).padStart(16, "a"), kind: "address", value: `a${n}@flood.example`, until });

it("names the block an add replaces", async () => {
  await unlocked();
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fill("domain", "PROVIDER.example");
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect(confirm.mock.calls[0]?.[0]).toContain(`unaffected. This replaces the existing automatic block (level 2) until ${at(auto.until)}. Only this exact domain`);
  fill("address", "spammer@bad.example");
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect(confirm.mock.calls[1]?.[0]).toContain("unaffected. This replaces the existing manual block with no expiry. If the list is full");
  fill("address", "other@bad.example");
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect(confirm.mock.calls[2]?.[0]).not.toContain("replaces");
  expect(writes()).toHaveLength(0);
});

it("says how many automatic blocks a manual add removed to make room", async () => {
  const later = Date.now() + 86_400_000, expired = Date.now() + 200;
  list = { blocks: [manual, auto, autoAddress(1, later), autoAddress(2, later), autoAddress(3, expired)], evidence: quiet };
  await unlocked();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fill("domain", "provider.example");
  const added = { ...manual, id: auto.id, kind: "domain", value: "provider.example", reason: "spam" };
  answer = () => json({ block: added });
  // The replaced automatic block and one that merely expired are not counted.
  await new Promise(r => setTimeout(r, 250));
  list = { blocks: [manual, added, autoAddress(2, later)], evidence: quiet };
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect((await screen.findByText(/^Blocked the domain provider\.example\./)).textContent).toBe("Blocked the domain provider.example. 1 automatic block was removed to make room.");
  cleanup();
  list = { blocks: [manual, auto, autoAddress(1, later), autoAddress(2, later)], evidence: quiet };
  await unlocked();
  fill("address", "x@bad.example");
  answer = () => json({ block: { ...manual, id: "2222222222222222", value: "x@bad.example" } });
  list = { blocks: [manual, { ...manual, id: "2222222222222222", value: "x@bad.example" }], evidence: quiet };
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect((await screen.findByText(/^Blocked the address/)).textContent).toBe("Blocked the address x@bad.example. 3 automatic blocks were removed to make room.");
});

it("says what still refuses the sender after a removal", async () => {
  const domainBlock = { ...manual, id: "3333333333333333", kind: "domain", value: "bad.example" };
  const second = { ...manual, id: "4444444444444444", value: "two@bad.example" };
  list = { blocks: [manual, second, domainBlock], evidence: quiet };
  await unlocked();
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(removeManual());
  expect(confirm.mock.calls[0]?.[0]).toBe("Remove the manual address block on spammer@bad.example? Mail from this address stays refused by the domain block on bad.example. Automatic blocks of this exact address are then suppressed for 30 days.");
  fireEvent.click(screen.getByRole("button", { name: "Remove domain block bad.example" }));
  expect(confirm.mock.calls[1]?.[0]).toBe("Remove the manual domain block on bad.example? Other mail from this domain is accepted again from now on; 2 address blocks on this domain stay in force. Automatic blocks of this exact domain are then suppressed for 30 days.");
  cleanup();
  list = { blocks: [manual, domainBlock], evidence: quiet };
  await unlocked();
  fireEvent.click(screen.getByRole("button", { name: "Remove domain block bad.example" }));
  expect(confirm.mock.calls[2]?.[0]).toContain("Other mail from this domain is accepted again from now on; 1 address block on this domain stays in force.");
  expect(writes()).toHaveLength(0);
});

it("escapes non-ASCII spaces and @ lookalikes that fake another domain", async () => {
  const value = `billing\uFF20partner.com${"\u3000".repeat(100)}x@evil.tld`;
  list = { blocks: [{ ...manual, value }, { ...manual, id: "5555555555555555", value: "a\u00A0b\u2003c\u202Fd\u205Fe\uFE6Bf\u2028g\u2029h@x.example" }], evidence: quiet };
  render(view());
  expect(await screen.findByText("billing[U+FF20]partner.com[U+3000 \u00D7100]x@evil.tld")).toBeDefined();
  expect(screen.getByText("a[U+00A0]b[U+2003]c[U+202F]d[U+205F]e[U+FE6B]f[U+2028]g[U+2029]h@x.example")).toBeDefined();
  expect(document.body.textContent).not.toMatch(/[\u00A0\u2003\u202F\u205F\u3000\uFF20\uFE6B\u2028\u2029]/);
});

it("refuses a past expiry and an unpaired surrogate before confirming", async () => {
  await unlocked();
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(true);
  fill("address", "x@bad.example", "2020-01-01T00:00");
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect(screen.getByRole("alert").textContent).toBe("Expiry must be in the future.");
  expect((screen.getByLabelText("Expires (optional; empty means no expiry)") as HTMLInputElement).min).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/);
  fill("address", "x\uD800@bad.example");
  fireEvent.click(screen.getByRole("button", { name: "Block sender…" }));
  expect(screen.getByRole("alert").textContent).toBe("The value contains an unpaired surrogate code unit, so it cannot be a sender.");
  expect(confirm).not.toHaveBeenCalled();
  expect(writes()).toHaveLength(0);
});

it("explains disabled actions and keeps the status regions mounted", async () => {
  render(view());
  await screen.findByText("spammer@bad.example");
  expect(screen.getByText("Enter your account password above to add or remove blocks.")).toBeDefined();
  expect((removeManual() as HTMLButtonElement).disabled).toBe(true);
  const regions = screen.getAllByRole("status");
  expect(regions).toHaveLength(2);
  expect(regions.every(r => r.textContent === "")).toBe(true);
  fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "account-secret" } });
  expect(screen.queryByText("Enter your account password above to add or remove blocks.")).toBeNull();
  vi.spyOn(window, "confirm").mockReturnValue(true);
  fireEvent.click(removeManual());
  await waitFor(() => expect(regions[0]!.textContent).toBe("Removed the manual address block on spammer@bad.example."));
  expect(regions[0]!.isConnected).toBe(true);
});
