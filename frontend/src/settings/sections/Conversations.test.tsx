import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { Conversations } from "./Conversations";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it("shows the server's choice and saves a change to the account", async () => {
  const fetchMock = vi.fn<typeof fetch>(async (_url, init) =>
    init?.method === "PUT" ? json({ conversations: false }) : json({ conversations: true }));
  vi.stubGlobal("fetch", fetchMock);
  render(<Conversations />);
  const box = await screen.findByRole("checkbox", { name: "Group messages into conversations" });
  expect((box as HTMLInputElement).checked).toBe(true);
  fireEvent.click(box);
  await waitFor(() => expect((box as HTMLInputElement).checked).toBe(false));
  const put = fetchMock.mock.calls.find(([, init]) => init?.method === "PUT");
  expect(put?.[0]).toBe("/api/settings/mail");
  expect(JSON.parse(String(put?.[1]?.body))).toEqual({ conversations: false });
});
