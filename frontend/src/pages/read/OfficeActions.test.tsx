import { afterEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { OfficeActions } from "./OfficeActions";
import { officeViewable } from "../../api/kydrive";

afterEach(() => { cleanup(); vi.unstubAllGlobals(); vi.restoreAllMocks(); });

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

function fakeTab() {
  const tab = { opener: {} as unknown, location: { replace: vi.fn() }, close: vi.fn() };
  vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
  return tab;
}

it("opens a read-only viewing copy in a tab with no opener", async () => {
  const tab = fakeTab();
  const fetchMock = vi.fn<typeof fetch>(async () => json({ viewUrl: "https://drive.example/view.html?copy=c1" }, 201));
  vi.stubGlobal("fetch", fetchMock);
  render(<OfficeActions messageId="5" folder="INBOX" index={1} name="plan.docx" origin="https://drive.example" />);
  fireEvent.click(screen.getByRole("button", { name: "Open plan.docx read-only" }));
  await waitFor(() => expect(tab.location.replace).toHaveBeenCalledWith("https://drive.example/view.html?copy=c1"));
  expect(tab.opener).toBeNull();
  const [url, init] = fetchMock.mock.calls[0];
  expect(url).toBe("/api/kydrive/view-attachment");
  expect(JSON.parse(String(init?.body))).toEqual({ messageId: "5", mailbox: "INBOX", index: 1 });
});

it("edits through save-attachment and opens the editor", async () => {
  const tab = fakeTab();
  const fetchMock = vi.fn<typeof fetch>(async () => json({ fileId: "f1", editUrl: "https://drive.example/editor.html?file=f1" }, 201));
  vi.stubGlobal("fetch", fetchMock);
  render(<OfficeActions messageId="5" folder="" index={0} name="plan.docx" origin="https://drive.example" />);
  fireEvent.click(screen.getByRole("button", { name: "Edit plan.docx in KyDrive" }));
  await waitFor(() => expect(tab.location.replace).toHaveBeenCalledWith("https://drive.example/editor.html?file=f1"));
  expect(fetchMock.mock.calls[0][0]).toBe("/api/kydrive/save-attachment");
  expect(JSON.parse(String(fetchMock.mock.calls[0][1]?.body))).toEqual({ messageId: "5", mailbox: "", index: 0, edit: true });
});

it("never follows an address off the paired KyDrive", async () => {
  const tab = fakeTab();
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async () => json({ viewUrl: "https://evil.example/view.html?copy=c1" }, 201)));
  render(<OfficeActions messageId="5" folder="" index={0} name="plan.docx" origin="https://drive.example" />);
  fireEvent.click(screen.getByRole("button", { name: "Open plan.docx read-only" }));
  expect(await screen.findByRole("alert")).toBeTruthy();
  expect(tab.location.replace).not.toHaveBeenCalled();
  expect(tab.close).toHaveBeenCalled();
});

it("explains a refusal", async () => {
  fakeTab();
  vi.stubGlobal("fetch", vi.fn<typeof fetch>(async () => json({ error: "x", code: "encrypted" }, 409)));
  render(<OfficeActions messageId="5" folder="" index={0} name="plan.docx" origin="https://drive.example" />);
  fireEvent.click(screen.getByRole("button", { name: "Open plan.docx read-only" }));
  expect((await screen.findByRole("alert")).textContent).toBe(" Attachments of encrypted mail stay on your device.");
});

it("decides from the extension and the declared type together", () => {
  const docx = "application/vnd.openxmlformats-officedocument.wordprocessingml.document";
  expect(officeViewable("plan.docx", docx)).toBe(true);
  expect(officeViewable("PLAN.DOCX", docx + "; name=x")).toBe(true);
  expect(officeViewable("scan.pdf", "application/octet-stream")).toBe(true);
  expect(officeViewable("plan.docx", "application/pdf")).toBe(false);
  expect(officeViewable("page.html", "text/html")).toBe(false);
  expect(officeViewable("run.exe", "application/octet-stream")).toBe(false);
  expect(officeViewable("docx", docx)).toBe(false);
  for (const ext of ["constructor", "__proto__", "toString", "hasOwnProperty"]) expect(officeViewable(`x.${ext}`, "text/plain")).toBe(false);
});
