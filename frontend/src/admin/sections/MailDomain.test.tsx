import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { AuthContext, type AuthState } from "../../auth";
import { deriveCredential } from "../../api/auth";
import { withSSOStepUp } from "../../api/stepup";
import { readMailDomain, readMailDomains, readMailRelay } from "../../api/nativeMail";
import { MailDomain } from "./MailDomain";

vi.mock("../../api/auth", async (original) => ({
  ...await original<typeof import("../../api/auth")>(),
  deriveCredential: vi.fn(),
}));
vi.mock("../../api/stepup", () => ({ withSSOStepUp: vi.fn() }));
const token = "a".repeat(64);
const configuredDomain = {
  configured: true, receivingEnabled: false,
  claim: { domain: "example.com", issuer: "https://identity.example.com", token, expiresAt: 1791200000, established: true, verifiedUntil: 1791110000 },
  recordName: "_kypost-mail.example.com", recordValue: `kypost-mail-verify=${token}`,
};
const emptyDomain = { configured: false, receivingEnabled: false, claim: { domain: "", issuer: "", token: "", expiresAt: 0 }, recordName: "", recordValue: "" };
const emptyRelay = { configured: false, domain: "", issuer: "", host: "", port: 0, generation: "", transport: "implicit-tls", authRequired: true, sendingEnabled: false };
const configuredRelay = { ...emptyRelay, configured: true, domain: "example.com", issuer: "https://identity.example.com", host: "smtp.example.com", port: 465, generation: "relay-generation", sendingEnabled: true };
let domainResponse: unknown, relayResponse: unknown, setResponse: unknown;
let failRead = false, failWrite = false;
const fetchMock = vi.fn<typeof fetch>();
const admin: AuthState = { authenticated: true, userId: "admin-id", username: "admin", role: "admin" };
function view(auth = admin) {
  return <MemoryRouter><AuthContext.Provider value={auth}><MailDomain /></AuthContext.Provider></MemoryRouter>;
}
function fill(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}
beforeEach(() => {
  domainResponse = configuredDomain; relayResponse = emptyRelay; setResponse = null;
  failRead = false; failWrite = false;
  vi.stubGlobal("fetch", fetchMock);
  fetchMock.mockImplementation(async (url, init) => {
    if (init?.method && init.method !== "GET") {
      // A rejected fetch is an answer that never arrived.
      if (failWrite) throw new TypeError("network connection lost");
      return new Response("{}");
    }
    if (failRead) return new Response("preserve unreadable configuration", { status: 503 });
    // null emulates a server without the domain-set API (single-domain fallback).
    if (url === "/api/admin/mail-domains" && setResponse === null) return new Response(JSON.stringify({ error: "not found" }), { status: 404, headers: { "Content-Type": "application/json" } });
    if (url === "/api/admin/mail-domains") return new Response(JSON.stringify(setResponse), { headers: { "Content-Type": "application/json" } });
    return new Response(JSON.stringify(url === "/api/admin/mail-domain" ? domainResponse : relayResponse), { headers: { "Content-Type": "application/json" } });
  });
  vi.mocked(deriveCredential).mockResolvedValue({ password: "must-not-transmit", authSecret: "derived-test-secret", loginSalt: "test-salt", loginIterations: 1, derivation: "pbkdf2" });
  vi.mocked(withSSOStepUp).mockImplementation(run => run({}));
  document.cookie = "csrf_token=test-csrf";
});
afterEach(() => { cleanup(); vi.resetAllMocks(); vi.unstubAllGlobals(); document.cookie = "csrf_token=; Max-Age=0"; });

it("validates untrusted status instead of inventing readiness", () => {
  expect(readMailDomain(emptyDomain)).toEqual({ kind: "unconfigured" });
  expect(readMailRelay(emptyRelay)).toEqual({ kind: "unconfigured" });
  for (const value of [null, {}, { ...configuredDomain, receivingEnabled: true }, { ...configuredDomain, recordValue: "foreign-token" }, { ...configuredDomain, claim: { ...configuredDomain.claim, expiresAt: "tomorrow" } }]) {
    expect(() => readMailDomain(value)).toThrow();
  }
  for (const value of [{ ...configuredRelay, transport: "starttls" }, { ...configuredRelay, port: 70000 }, { ...configuredRelay, sendingEnabled: "yes" }]) {
    expect(() => readMailRelay(value)).toThrow();
  }
});

it("creates a challenge using the shared derived credential and CSRF client", async () => {
  domainResponse = emptyDomain;
  render(view());
  const button = await screen.findByRole("button", { name: "Create TXT challenge" });
  expect(button.hasAttribute("disabled")).toBe(true);
  fill("Domain", "example.com"); fill("Account password", "account-secret");
  domainResponse = configuredDomain;
  fireEvent.click(button);
  await screen.findByLabelText("TXT value");
  const write = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(write?.[0]).toBe("/api/admin/mail-domain");
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ domain: "example.com", authSecret: "derived-test-secret" });
  expect(write?.[1]?.credentials).toBe("include");
  expect(new Headers(write?.[1]?.headers).get("X-CSRF-Token")).toBe("test-csrf");
  expect(deriveCredential).toHaveBeenCalledWith("", "account-secret");
  expect(screen.getByLabelText("Account password").getAttribute("value")).toBe("");
});

it("disables changes on unreadable status or mismatched relay ownership", async () => {
  failRead = true;
  const rendered = render(view());
  await screen.findByRole("alert");
  expect(screen.getByRole("button", { name: "Add domain" }).closest("fieldset")?.disabled).toBe(true);
  rendered.unmount(); failRead = false; relayResponse = { ...configuredRelay, domain: "foreign.example" };
  render(view());
  expect((await screen.findByRole("alert")).textContent).toContain("ownership differ");
  expect(screen.getByRole("button", { name: "Save outgoing relay" }).closest("fieldset")?.disabled).toBe(true);
});

it("requires explicit confirmation before rotating the fixed domain challenge", async () => {
  render(view()); await screen.findByLabelText("TXT name");
  fill("Account password", "account-secret");
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(screen.getByRole("button", { name: "Replace challenge" }));
  expect(confirm).toHaveBeenCalled(); expect(deriveCredential).not.toHaveBeenCalled();
  confirm.mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Replace challenge" }));
  await screen.findByRole("status");
  await waitFor(() => expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PUT")).toBe(true));
  const write = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ domain: "example.com", authSecret: "derived-test-secret" });
  confirm.mockRestore();
});

it("freezes SSO relay replay, clears secrets and never calls saving a delivery test", async () => {
  render(view({ ...admin, ssoSession: true }));
  await screen.findByLabelText("TXT name");
  expect(screen.queryByLabelText("Account password")).toBeNull();
  fill("Relay host", "smtp.example.com"); fill("Relay username", "provider-login"); fill("Relay password", "provider-secret");
  vi.mocked(withSSOStepUp).mockImplementation(async run => {
    await run({});
    return run({ "X-Kypost-Step-Up": "test-grant" });
  });
  relayResponse = configuredRelay;
  fireEvent.click(screen.getByRole("button", { name: "Save outgoing relay" }));
  await screen.findByText("Relay saved. No provider connection or test email was made.");
  const writes = fetchMock.mock.calls.filter(([, init]) => init?.method === "PUT");
  expect(writes).toHaveLength(2);
  expect(writes[0]?.[1]?.body).toBe(writes[1]?.[1]?.body);
  expect(JSON.parse(String(writes[0]?.[1]?.body))).toEqual({ host: "smtp.example.com", port: 465, smtpUsername: "provider-login", smtpPassword: "provider-secret" });
  expect(new Headers(writes[1]?.[1]?.headers).get("X-Kypost-Step-Up")).toBe("test-grant");
  expect(deriveCredential).not.toHaveBeenCalled();
  expect(screen.getByLabelText("Relay password").getAttribute("value")).toBe("");
  expect(screen.getByLabelText("Relay username").getAttribute("value")).toBe("");
  expect(screen.getByText(/Provider delivery remains untested/)).toBeDefined();
  expect(screen.getByText(/Public receiving is not ready/)).toBeDefined();
});

it("refuses an SSO replay after the account changes", async () => {
  let replay: (() => Promise<unknown>) | undefined;
  vi.mocked(withSSOStepUp).mockImplementation(run => new Promise(resolve => {
    replay = async () => { const result = await run({ "X-Kypost-Step-Up": "stale-grant" }); resolve(result); return result; };
  }));
  const rendered = render(view({ ...admin, ssoSession: true }));
  await screen.findByLabelText("TXT name");
  fireEvent.click(screen.getByRole("button", { name: "Verify TXT record" }));
  await waitFor(() => expect(replay).toBeDefined());
  rendered.rerender(view({ ...admin, userId: "other-admin", ssoSession: true }));
  await expect(replay?.()).rejects.toThrow("Mail setup closed");
  expect(fetchMock.mock.calls.some(([, init]) => init?.method === "POST")).toBe(false);
});

it("clears credentials and stays locked after a write whose answer never arrived", async () => {
  render(view()); await screen.findByLabelText("TXT name");
  fill("Account password", "account-secret"); fill("Relay host", "smtp.example.com");
  fill("Relay username", "provider-login"); fill("Relay password", "provider-secret");
  failWrite = true;
  fireEvent.click(screen.getByRole("button", { name: "Save outgoing relay" }));
  await screen.findByRole("alert");
  expect(screen.getByLabelText("Relay password").getAttribute("value")).toBe("");
  expect(screen.getByLabelText("Account password").getAttribute("value")).toBe("");
  expect(screen.getByRole("button", { name: "Save outgoing relay" }).closest("fieldset")?.disabled).toBe(true);
});

it("warns before replacing a configured relay and leaves cancelled credentials local", async () => {
  relayResponse = configuredRelay;
  render(view()); await screen.findByLabelText("TXT name");
  fill("Account password", "account-secret"); fill("Relay username", "new-login"); fill("Relay password", "new-secret");
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fireEvent.click(screen.getByRole("button", { name: "Save outgoing relay" }));
  expect(confirm.mock.calls[0]?.[0]).toContain("stops unclaimed deliveries");
  expect(deriveCredential).not.toHaveBeenCalled();
  expect(fetchMock.mock.calls.some(([, init]) => init?.method === "PUT")).toBe(false);
  confirm.mockReturnValue(true);
  fireEvent.click(screen.getByRole("button", { name: "Save outgoing relay" }));
  await screen.findByText("Relay saved. No provider connection or test email was made.");
  expect(fetchMock.mock.calls.filter(([, init]) => init?.method === "PUT")).toHaveLength(1);
  confirm.mockRestore();
});

it("checks only the saved generation using derived confirmation and CSRF", async () => {
  relayResponse = configuredRelay;
  const result = { generation: configuredRelay.generation, host: configuredRelay.host, port: configuredRelay.port, tls: true, authenticated: true, deliveryTested: false };
  const read = fetchMock.getMockImplementation();
  fetchMock.mockImplementation(async (url, init) => init?.method === "POST" ? new Response(JSON.stringify(result)) : read!(url, init));
  render(view()); await screen.findByLabelText("TXT name");
  fill("Account password", "account-secret"); fill("Relay host", "unsaved.example.com");
  fill("Relay username", "unsaved-login"); fill("Relay password", "unsaved-secret");
  fireEvent.click(screen.getByRole("button", { name: "Check saved relay" }));
  await screen.findByText("TLS and SMTP authentication passed for the saved relay. No email was sent; delivery remains untested.");
  const write = fetchMock.mock.calls.find(([, init]) => init?.method === "POST");
  expect(write?.[0]).toBe("/api/admin/mail-relay/test");
  expect(JSON.parse(String(write?.[1]?.body))).toEqual({ expectedGeneration: configuredRelay.generation, authSecret: "derived-test-secret" });
  expect(new Headers(write?.[1]?.headers).get("X-CSRF-Token")).toBe("test-csrf");
  expect(screen.getByLabelText("Relay password").getAttribute("value")).toBe("");
  expect(screen.getByText(/Provider delivery remains untested/)).toBeDefined();
});

it("freezes the saved relay check through SSO confirmation and rejects changed status", async () => {
  relayResponse = configuredRelay;
  const result = { generation: configuredRelay.generation, host: configuredRelay.host, port: configuredRelay.port, tls: true, authenticated: true, deliveryTested: false };
  const read = fetchMock.getMockImplementation();
  fetchMock.mockImplementation(async (url, init) => init?.method === "POST" ? new Response(JSON.stringify(result)) : read!(url, init));
  vi.mocked(withSSOStepUp).mockImplementation(async run => { await run({}); relayResponse = { ...configuredRelay, generation: "rotated-profile" }; return run({ "X-Kypost-Step-Up": "test-grant" }); });
  render(view({ ...admin, ssoSession: true })); await screen.findByLabelText("TXT name");
  fireEvent.click(screen.getByRole("button", { name: "Check saved relay" }));
  expect((await screen.findByRole("alert")).textContent).toContain("did not match the saved profile");
  const writes = fetchMock.mock.calls.filter(([, init]) => init?.method === "POST");
  expect(writes).toHaveLength(2); expect(writes[0]?.[1]?.body).toBe(writes[1]?.[1]?.body);
  expect(JSON.parse(String(writes[0]?.[1]?.body))).toEqual({ expectedGeneration: configuredRelay.generation });
  expect(screen.queryByText(/TLS and SMTP authentication passed/)).toBeNull();
  expect(screen.getByRole("button", { name: "Save outgoing relay" }).closest("fieldset")?.disabled).toBe(true);
});

it("refuses a relay check response claiming mail delivery or omitting authentication", async () => {
  const { readMailRelayCheck } = await import("../../api/nativeMail");
  const relay = readMailRelay(configuredRelay);
  const good = { generation: configuredRelay.generation, host: configuredRelay.host, port: configuredRelay.port, tls: true, authenticated: true, deliveryTested: false };
  expect(() => readMailRelayCheck(good, relay)).not.toThrow();
  for (const result of [null, {}, { ...good, generation: "other" }, { ...good, host: "other.example" }, { ...good, port: 587 }, { ...good, tls: false }, { ...good, authenticated: false }, { ...good, deliveryTested: true }]) {
    expect(() => readMailRelayCheck(result, relay)).toThrow();
  }
});

const issuer = "https://identity.example.com";
function entry(domain: string, extra: object = {}) {
  return { domain, configured: true, retired: false, recordName: `_kypost-mail.${domain}`, recordValue: `kypost-mail-verify=${token}`, established: true, expiresAt: 1791200000, verifiedUntil: 0, ...extra };
}
const retiredEntry = { domain: "old.example", configured: false, retired: true, recordName: "", recordValue: "", established: false, expiresAt: 0, verifiedUntil: 0 };
const domainSet = { issuer, founding: "example.com", receivingEnabled: false, domains: [
  entry("example.com"), entry("second.example"),
  entry("pending.example", { established: false, expiresAt: 4102444800 }),
  entry("lapsed.example", { established: false, expiresAt: 1 }),
  retiredEntry,
] };
const setRelay = { ...configuredRelay, domains: ["example.com"], retiredDomains: ["gone.example"] };
const writes = (method: string) => fetchMock.mock.calls.filter(([, init]) => init?.method === method);
const region = (domain: string) => within(screen.getByRole("heading", { name: domain }).closest("li")!);

it("validates the domain set and relay domains at the boundary", () => {
  expect(readMailDomains({ issuer: "", founding: "", domains: [], receivingEnabled: false })).toEqual({ issuer: "", founding: "", domains: [] });
  expect(readMailDomains(domainSet).domains.map(d => d.retired)).toEqual([false, false, false, false, true]);
  for (const value of [
    { ...domainSet, receivingEnabled: true }, { ...domainSet, founding: "old.example" }, { ...domainSet, founding: "" },
    { ...domainSet, domains: [entry("example.com", { recordValue: "foreign" })] },
    { ...domainSet, domains: [entry("example.com"), { ...retiredEntry, recordValue: `kypost-mail-verify=${token}` }] },
    { ...domainSet, domains: [entry("example.com", { configured: false })] },
    { ...domainSet, domains: [entry("example.com"), entry("example.com")] },
  ]) expect(() => readMailDomains(value)).toThrow();
  expect(readMailRelay(configuredRelay)).toMatchObject({ domains: ["example.com"], retiredDomains: [] });
  expect(() => readMailRelay({ ...setRelay, domains: ["other.example"] })).toThrow();
});

it("lists every domain status with its record, founding mark and actions", async () => {
  setResponse = domainSet;
  render(view()); await screen.findByRole("heading", { name: "example.com" });
  expect(region("example.com").getByText("Founding domain.")).toBeDefined();
  expect(screen.getByRole("list", { name: "Mail domains" }).children).toHaveLength(5);
  expect(region("example.com").getByText("Established")).toBeDefined();
  expect(region("pending.example").getByText("Awaiting verification")).toBeDefined();
  expect(region("pending.example").getByLabelText("TXT name").getAttribute("value")).toBe("_kypost-mail.pending.example");
  expect(region("pending.example").getByLabelText("TXT value").textContent).toBe(`kypost-mail-verify=${token}`);
  expect(region("pending.example").getByText(/verify it before/)).toBeDefined();
  expect(region("lapsed.example").getByText("Lapsed")).toBeDefined();
  expect(region("old.example").getByText("Retired")).toBeDefined();
  expect(region("old.example").queryByLabelText("TXT name")).toBeNull();
  expect(region("old.example").getByRole("button", { name: "Re-add old.example" })).toBeDefined();
  expect(region("old.example").queryByRole("button", { name: /Retire/ })).toBeNull();
  expect(screen.getAllByText(issuer)).toHaveLength(1);
  expect(screen.queryByText(/Connect one domain|fixed for this stack/)).toBeNull();
  expect(fetchMock.mock.calls.some(([url]) => url === "/api/admin/mail-domain")).toBe(false);
});

it("adds, verifies, retires and re-adds domains through confirmed protected requests", async () => {
  setResponse = domainSet;
  render(view()); await screen.findByRole("heading", { name: "example.com" });
  const step = async (click: () => void) => {
    fill("Account password", "account-secret");
    const before = fetchMock.mock.calls.length;
    click();
    await waitFor(() => expect(fetchMock.mock.calls.length).toBeGreaterThan(before + 1));
    await screen.findByRole("heading", { name: "example.com" });
    const [url, init] = fetchMock.mock.calls[before]!;
    return { url, method: init?.method, body: JSON.parse(String(init?.body)) };
  };
  fill("Domain", "new.example");
  expect(await step(() => fireEvent.click(screen.getByRole("button", { name: "Add domain" }))))
    .toEqual({ url: "/api/admin/mail-domains", method: "POST", body: { domain: "new.example", authSecret: "derived-test-secret" } });
  expect(await step(() => fireEvent.click(screen.getByRole("button", { name: "Verify pending.example" }))))
    .toEqual({ url: "/api/admin/mail-domains/pending.example/verify", method: "POST", body: { authSecret: "derived-test-secret" } });
  expect(await step(() => fireEvent.click(screen.getByRole("button", { name: "Re-add old.example" }))))
    .toEqual({ url: "/api/admin/mail-domains", method: "POST", body: { domain: "old.example", authSecret: "derived-test-secret" } });
  const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
  fill("Account password", "account-secret");
  fireEvent.click(screen.getByRole("button", { name: "Retire second.example" }));
  expect(confirm.mock.calls[0]?.[0]).toMatch(/refuses while any address on it is active.*relay still sends for it.*address records are kept.*re-add it later.*fresh DNS proof/);
  expect(writes("DELETE")).toHaveLength(0);
  confirm.mockReturnValue(true);
  expect(await step(() => fireEvent.click(screen.getByRole("button", { name: "Retire second.example" }))))
    .toEqual({ url: "/api/admin/mail-domains/second.example", method: "DELETE", body: { authSecret: "derived-test-secret" } });
  expect(new Headers(writes("DELETE")[0]?.[1]?.headers).get("X-CSRF-Token")).toBe("test-csrf");
  confirm.mockReturnValue(false);
  fill("Account password", "account-secret");
  fireEvent.click(screen.getByRole("button", { name: "Replace challenge for example.com" }));
  expect(confirm.mock.calls[confirm.mock.calls.length - 1]?.[0]).toContain("for example.com");
  expect(writes("POST")).toHaveLength(3);
  confirm.mockRestore();
});

it("replays an identical retire request through KySignOn step-up", async () => {
  setResponse = domainSet;
  vi.spyOn(window, "confirm").mockReturnValue(true);
  vi.mocked(withSSOStepUp).mockImplementation(async run => { await run({}); return run({ "X-Kypost-Step-Up": "test-grant" }); });
  render(view({ ...admin, ssoSession: true })); await screen.findByRole("heading", { name: "example.com" });
  fireEvent.click(screen.getByRole("button", { name: "Retire second.example" }));
  await screen.findByText(/second.example retired/);
  const deletes = writes("DELETE");
  expect(deletes).toHaveLength(2);
  expect(deletes[0]?.[1]?.body).toBe(deletes[1]?.[1]?.body);
  expect(JSON.parse(String(deletes[0]?.[1]?.body))).toEqual({});
  expect(new Headers(deletes[1]?.[1]?.headers).get("X-Kypost-Step-Up")).toBe("test-grant");
  expect(deriveCredential).not.toHaveBeenCalled();
});

it("shows server refusals and returns controls once status re-reads", async () => {
  setResponse = domainSet;
  const inUse = "mail domain still in use by an active address, a queued or retryable outbox job, held incoming mail or the relay; disable those addresses, let the mail finish and remove the domain from the relay first, then retry";
  const read = fetchMock.getMockImplementation();
  const verifyRefused = "mail domain verification failed; check the domain is configured, the exact TXT record, challenge expiry and DNS availability";
  fetchMock.mockImplementation(async (url, init) => init?.method === "DELETE" ? new Response(inUse, { status: 409 })
    : init?.method === "POST" ? new Response(verifyRefused, { status: 409 }) : read!(url, init));
  vi.spyOn(window, "confirm").mockReturnValue(true);
  render(view()); await screen.findByRole("heading", { name: "example.com" });
  const fieldset = () => screen.getByRole("button", { name: "Add domain" }).closest("fieldset");
  for (const [button, message] of [["Retire example.com", inUse], ["Verify pending.example", verifyRefused]] as const) {
    fill("Account password", "account-secret");
    fireEvent.click(screen.getByRole("button", { name: button }));
    await waitFor(() => expect(screen.getByRole("alert").textContent).toBe(`request failed: 409 - ${message}`));
    await waitFor(() => expect(fieldset()?.disabled).toBe(false));
    expect(screen.getByRole("alert").textContent).toBe(`request failed: 409 - ${message}`);
  }
});

it("refuses to add a domain that is already in service", async () => {
  setResponse = domainSet;
  render(view()); await screen.findByRole("heading", { name: "example.com" });
  fill("Account password", "account-secret"); fill("Domain", " Second.Example ");
  const add = screen.getByRole("button", { name: "Add domain" });
  expect(add.hasAttribute("disabled")).toBe(true);
  expect(screen.getByText(/second.example is already configured; use its Replace challenge/)).toBeDefined();
  fireEvent.click(add);
  expect(writes("POST")).toHaveLength(0);
  fill("Domain", "old.example");
  expect(add.hasAttribute("disabled")).toBe(false);
});

it("saves relay sending domains alone without relay credentials", async () => {
  setResponse = domainSet; relayResponse = setRelay;
  render(view()); await screen.findByRole("heading", { name: "example.com" });
  const group = within(screen.getByRole("group", { name: "Relay sending domains" }));
  expect((group.getByLabelText("example.com") as HTMLInputElement).checked).toBe(true);
  expect((group.getByLabelText(/pending.example/) as HTMLInputElement).disabled).toBe(true);
  expect(group.queryByLabelText("old.example")).toBeNull();
  expect(group.getByText(/gone.example/)).toBeDefined();
  expect(group.getByText(/needs a current DNS proof/)).toBeDefined();
  expect(group.getByText(/refused while queued or retryable mail/)).toBeDefined();
  fill("Account password", "account-secret");
  expect(screen.getByRole("button", { name: "Retire second.example" }).hasAttribute("disabled")).toBe(false);
  expect(screen.getByRole("button", { name: "Retire example.com" }).hasAttribute("disabled")).toBe(true);
  expect(region("example.com").getByText(/remove it from the relay first/)).toBeDefined();
  const save = group.getByRole("button", { name: "Save sending domains" });
  expect(save.hasAttribute("disabled")).toBe(true);
  fireEvent.click(group.getByLabelText("second.example"));
  relayResponse = { ...setRelay, domains: ["example.com", "second.example"] };
  fireEvent.click(save);
  await screen.findByText("Relay sending domains saved. Credentials and relay generation are unchanged.");
  const puts = writes("PUT");
  expect(puts).toHaveLength(1);
  expect(puts[0]?.[0]).toBe("/api/admin/mail-relay");
  expect(JSON.parse(String(puts[0]?.[1]?.body))).toEqual({ domains: ["example.com", "second.example"], authSecret: "derived-test-secret" });
});

it("refuses a relay that sends for a domain outside the configured set", async () => {
  setResponse = domainSet; relayResponse = { ...setRelay, domains: ["example.com", "old.example"] };
  render(view());
  expect((await screen.findByRole("alert")).textContent).toContain("ownership differ");
});

it("falls back to the single-domain view when the domain-set API is absent", async () => {
  render(view()); await screen.findByLabelText("TXT name");
  expect(fetchMock.mock.calls.map(([url]) => url)).toEqual(expect.arrayContaining(["/api/admin/mail-domains", "/api/admin/mail-domain"]));
  expect(screen.getByRole("button", { name: "Verify TXT record" })).toBeDefined();
  expect(screen.queryByRole("button", { name: "Add domain" })).toBeNull();
  expect(screen.queryByRole("group", { name: "Relay sending domains" })).toBeNull();
});

it("shows the storage migration remediation instead of falling back", async () => {
  const migration = "native mail storage migration is pending or failed, so native mail is refused; read the kypost-server migrate-native error in the container log, fix it and restart, or restore the pre-upgrade backup (docs/RESTORE.md#storage-format-migration)";
  const read = fetchMock.getMockImplementation();
  fetchMock.mockImplementation(async (url, init) => url === "/api/admin/mail-domains" ? new Response(migration, { status: 503 }) : read!(url, init));
  render(view());
  expect((await screen.findByRole("alert")).textContent).toBe(`request failed: 503 - ${migration}`);
  expect(fetchMock.mock.calls.some(([url]) => url === "/api/admin/mail-domain")).toBe(false);
  expect(screen.getByRole("button", { name: "Add domain" }).closest("fieldset")?.disabled).toBe(true);
});
