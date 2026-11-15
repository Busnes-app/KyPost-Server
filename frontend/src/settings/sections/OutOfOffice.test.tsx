import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { OutOfOffice } from "./OutOfOffice";
import { OutOfOfficeBar } from "../../components/OutOfOfficeBar";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const view = (patch = {}) => ({
  enabled: false, start: "", end: "", timeZone: "", localMessage: "", remote: "off", remoteMessage: "",
  active: false, remoteAllowed: false, ...patch
});

function stub(get: object, put?: (body: Record<string, unknown>) => Response) {
  const fetchMock = vi.fn<typeof fetch>(async (_url, init) =>
    init?.method === "PUT" ? (put ?? ((b) => json(view(b))))(JSON.parse(String(init.body))) : json(get));
  vi.stubGlobal("fetch", fetchMock);
  return fetchMock;
}

it("saves the reply with its period as instants", async () => {
  const fetchMock = stub(view());
  render(<OutOfOffice />);
  fireEvent.click(await screen.findByRole("checkbox", { name: "Send automatic replies" }));
  fireEvent.change(screen.getByLabelText("Start (optional)"), { target: { value: "2026-12-01T09:00" } });
  fireEvent.change(screen.getByLabelText("Message for people in your organisation"), { target: { value: "Away until Monday." } });
  fireEvent.click(screen.getByRole("button", { name: "Save automatic reply" }));
  expect(await screen.findByText("Automatic reply saved.")).toBeTruthy();
  const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(put?.[0]).toBe("/api/mail/out-of-office");
  const body = JSON.parse(String(put?.[1]?.body));
  expect(body).toMatchObject({ enabled: true, end: "", localMessage: "Away until Monday.", remote: "off" });
  expect(body.start).toBe(new Date("2026-12-01T09:00").toISOString());
  expect(Object.keys(body).sort()).toEqual(["enabled", "end", "localMessage", "remote", "remoteMessage", "start", "timeZone"]);
});

it("hides the outside reply unless the administrator allows it", async () => {
  stub(view({ remote: "on", remoteMessage: "x" }));
  render(<OutOfOffice />);
  expect(await screen.findByText(/has not allowed automatic replies to people outside/)).toBeTruthy();
  expect(screen.queryByLabelText("People outside your organisation")).toBeNull();
  cleanup();
  stub(view({ remoteAllowed: true }));
  render(<OutOfOffice />);
  fireEvent.change(await screen.findByLabelText("People outside your organisation"), { target: { value: "contacts" } });
  expect(screen.getByLabelText("Message for people outside your organisation")).toBeTruthy();
});

it("shows the server's refusal", async () => {
  stub(view(), () => json({ error: "write the message before turning the reply on" }, 400));
  render(<OutOfOffice />);
  fireEvent.click(await screen.findByRole("button", { name: "Save automatic reply" }));
  expect(await screen.findByText(/Could not save your automatic reply/)).toBeTruthy();
});

it("the inbox bar turns the reply off in one click", async () => {
  const fetchMock = stub(view({ enabled: true, active: true, localMessage: "Away" }));
  render(<OutOfOfficeBar />);
  expect(await screen.findByText(/Out of office is on/)).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Turn off" }));
  await waitFor(() => expect(screen.queryByText(/Out of office is on/)).toBeNull());
  const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(JSON.parse(String(put?.[1]?.body))).toMatchObject({ enabled: false, localMessage: "Away" });
});

it("the inbox bar stays hidden while the reply is off", async () => {
  const fetchMock = stub(view());
  render(<OutOfOfficeBar />);
  await waitFor(() => expect(fetchMock).toHaveBeenCalled());
  expect(screen.queryByRole("status")).toBeNull();
});
