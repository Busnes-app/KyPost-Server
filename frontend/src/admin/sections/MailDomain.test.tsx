import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { AuthContext, type AuthState } from "../../auth";
import { deriveCredential } from "../../api/auth";
import { withSSOStepUp } from "../../api/stepup";
import { readMailDomain, readMailRelay } from "../../api/nativeMail";
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
let domainResponse: unknown, relayResponse: unknown;
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
  domainResponse = configuredDomain; relayResponse = emptyRelay;
  failRead = false; failWrite = false;
  vi.stubGlobal("fetch", fetchMock);
  fetchMock.mockImplementation(async (url, init) => {
    if (init?.method && init.method !== "GET") {
      return new Response(failWrite ? "uncertain request" : "{}", { status: failWrite ? 503 : 200 });
    }
    if (failRead) return new Response("preserve unreadable configuration", { status: 503 });
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
  expect(screen.getByRole("button", { name: "Create TXT challenge" }).closest("fieldset")?.disabled).toBe(true);
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

it("clears credentials and disables changes after an uncertain write", async () => {
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
