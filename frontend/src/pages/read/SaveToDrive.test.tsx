import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { SaveToDrive } from "./SaveToDrive";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

it("saves the attachment by index and says where it went", async () => {
  const fetchMock = vi.fn<typeof fetch>(async () => json({ fileId: "f1", name: "scan (2).pdf", size: 3, folder: "Mail attachments", appUrl: "https://drive.example.com/" }, 201));
  vi.stubGlobal("fetch", fetchMock);
  render(<SaveToDrive messageId="5" folder="INBOX" index={1} name="scan.pdf" />);
  fireEvent.click(screen.getByRole("button", { name: "Save scan.pdf to KyDrive" }));
  expect((await screen.findByRole("status")).textContent).toBe("Saved to KyDrive in Mail attachments as scan (2).pdf. Open KyDrive");
  expect(screen.getByRole("link", { name: "Open KyDrive" }).getAttribute("href")).toBe("https://drive.example.com/");
  const [url, init] = fetchMock.mock.calls[0];
  expect(url).toBe("/api/kydrive/save-attachment");
  expect(JSON.parse(String(init?.body))).toEqual({ messageId: "5", mailbox: "INBOX", index: 1 });
});

it("explains a refusal and lets the user try again", async () => {
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async () => json({ error: "x", code: "quota" }, 507)));
  render(<SaveToDrive messageId="5" folder="" index={0} name="a.pdf" />);
  fireEvent.click(screen.getByRole("button", { name: "Save a.pdf to KyDrive" }));
  expect((await screen.findByRole("alert")).textContent).toBe(" Your KyDrive is full.");
  expect(screen.getByRole("button", { name: "Save a.pdf to KyDrive" })).toBeTruthy();
});
