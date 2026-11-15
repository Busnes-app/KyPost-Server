import { afterEach, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { useState } from "react";
import type Quill from "quill";
import type { ComposeAttachment } from "../app/types";
import { HttpError } from "../api/client";

const api = vi.hoisted(() => ({
  status: { available: true, origin: "https://drive.example.com", appUrl: "https://drive.example.com/" } as Record<string, unknown>,
  ticket: vi.fn(async () => ({ uploadUrl: "https://drive.example.com/api/suite/upload/t", expires: "", maxChunk: 64 })),
  upload: vi.fn(async (_t: unknown, blob: Blob, onProgress: (n: number) => void, _signal: AbortSignal): Promise<{ file_id: string; name: string; size: number }> => { onProgress(blob.size); return { file_id: "f1", name: "big.bin", size: blob.size }; }),
  link: vi.fn(async () => ({ url: "https://drive.example.com/l/#tok", expires: "2026-12-14T00:00:00Z" })),
  outside: vi.fn(async (_a: string[]): Promise<string[]> => [])
}));
vi.mock("../api/kydrive", async (importActual) => ({
  ...(await importActual<typeof import("../api/kydrive")>()),
  loadDriveStatus: async () => api.status,
  requestUploadTicket: api.ticket,
  uploadToTicket: api.upload,
  createDriveLink: api.link,
  checkOutsideRecipients: (a: string[]) => api.outside(a)
}));

import { useComposeDrive } from "./useComposeDrive";

afterEach(() => vi.clearAllMocks());

/** A Quill stand-in: a contenteditable root and the two calls the hook makes. */
function fakeQuill() {
  const root = document.createElement("div");
  const inserted: Array<[string, unknown]> = [];
  return {
    root, inserted,
    getSelection: () => null,
    getLength: () => 1,
    insertText: (_at: number, text: string, formats: unknown) => {
      inserted.push([text, formats]);
      const link = (formats as { link?: unknown }).link;
      root.insertAdjacentHTML("beforeend", typeof link === "string" ? `<p><a href="${link}">${text}</a></p>` : "");
    },
    update: vi.fn()
  };
}

function setup(initial: ComposeAttachment[] = []) {
  const quill = fakeQuill();
  const setError = vi.fn();
  const view = renderHook(() => {
    const [attachments, setAttachments] = useState(initial);
    const drive = useComposeDrive({ composeOpen: true, quill: { current: quill as unknown as Quill }, attachments, setAttachments, setError });
    return { drive, attachments };
  });
  return { quill, setError, view };
}

const file = (name: string, size: number) => new File([new Uint8Array(size)], name);

it("offers KyDrive for an oversize pick only when it is available, and links the upload", async () => {
  const { quill, view } = setup();
  await waitFor(() => expect(view.result.current.drive.driveStatus.available).toBe(true));
  act(() => { expect(view.result.current.drive.offerDrive([file("big.bin", 10)])).toBe(true); });
  expect(view.result.current.drive.driveOffer).toHaveLength(1);
  await act(() => view.result.current.drive.acceptDriveOffer());
  expect(api.ticket).toHaveBeenCalledWith("big.bin", 10);
  expect(quill.inserted[0]).toEqual(["big.bin", { link: "https://drive.example.com/l/#tok" }]);
  expect(String(quill.inserted[1][0])).toMatch(/^ \(10 B, KyDrive, link expires .+\)\n$/);
  expect(view.result.current.drive.driveUploads).toEqual([]);

  api.status = { available: false };
  const off = setup();
  await waitFor(() => expect(api.ticket).toHaveBeenCalledTimes(1));
  act(() => { expect(off.view.result.current.drive.offerDrive([file("big.bin", 10)])).toBe(false); });
  api.status = { available: true, origin: "https://drive.example.com", appUrl: "https://drive.example.com/" };
});

it("leaves the draft as it was when an upload fails, and keeps the attachment", async () => {
  const attachment = { name: "a.txt", mimeType: "text/plain", dataBase64: btoa("hi"), size: 2 };
  const { quill, setError, view } = setup([attachment]);
  await waitFor(() => expect(view.result.current.drive.driveStatus.available).toBe(true));
  api.upload.mockRejectedValueOnce(new Error("network down"));
  await act(() => view.result.current.drive.attachmentToDrive(0));
  expect(setError).toHaveBeenCalledWith("Upload to KyDrive failed: network down");
  expect(quill.inserted).toEqual([]);
  expect(view.result.current.attachments).toEqual([attachment]);

  await act(() => view.result.current.drive.attachmentToDrive(0));
  expect(view.result.current.attachments).toEqual([]);
  expect(quill.inserted[0][0]).toBe("big.bin");
});

it("says nothing when the user cancels an upload", async () => {
  const { setError, view } = setup();
  await waitFor(() => expect(view.result.current.drive.driveStatus.available).toBe(true));
  api.upload.mockImplementationOnce((_t: unknown, _b: Blob, _p: (n: number) => void, signal: AbortSignal) => new Promise((_, reject) => signal.addEventListener("abort", () => reject(new DOMException("x", "AbortError")))));
  act(() => { view.result.current.drive.offerDrive([file("big.bin", 10)]); });
  let done!: Promise<void>;
  act(() => { done = view.result.current.drive.acceptDriveOffer(); });
  await waitFor(() => expect(view.result.current.drive.driveUploads).toHaveLength(1));
  act(() => view.result.current.drive.cancelDriveUpload(view.result.current.drive.driveUploads[0].id));
  await act(() => done);
  expect(setError).not.toHaveBeenCalled();
  expect(api.link).not.toHaveBeenCalled();
});

it("holds a send with KyDrive links to people outside the organisation", async () => {
  const { quill, view } = setup();
  await waitFor(() => expect(view.result.current.drive.driveStatus.available).toBe(true));
  const plain = "<p>no links</p>";
  expect(await view.result.current.drive.driveRecipientsOk(plain, ["bob@example.net"])).toBe(true);
  expect(api.outside).not.toHaveBeenCalled();

  act(() => { view.result.current.drive.offerDrive([file("big.bin", 10)]); });
  await act(() => view.result.current.drive.acceptDriveOffer());
  const body = quill.root.innerHTML;
  api.outside.mockResolvedValueOnce([]);
  expect(await view.result.current.drive.driveRecipientsOk(body, ["ann@example.com"])).toBe(true);

  api.outside.mockResolvedValueOnce(["bob@example.net"]);
  let ok = true;
  await act(async () => { ok = await view.result.current.drive.driveRecipientsOk(body, ["ann@example.com", "bob@example.net"]); });
  expect(ok).toBe(false);
  expect(api.outside).toHaveBeenLastCalledWith(["ann@example.com", "bob@example.net"]);
  expect(view.result.current.drive.driveOutside).toEqual({ addresses: ["bob@example.net"], unverified: false, canAttach: true, canGuest: false, sendAt: undefined });

  // "Attach instead": the link line goes and the file is attached.
  await act(() => view.result.current.drive.attachDriveFilesInstead());
  expect(quill.root.querySelector("a")).toBeNull();
  expect(view.result.current.attachments.map((a) => a.name)).toEqual(["big.bin"]);
  expect(view.result.current.drive.driveOutside).toBeNull();

  // A check that fails holds the send too, naming everyone; a reloaded draft's link cannot be attached.
  api.outside.mockRejectedValueOnce(new Error("down"));
  const reloaded = `<p><a href="https://drive.example.com/l/#old">old.pdf</a></p>`;
  await act(async () => { ok = await view.result.current.drive.driveRecipientsOk(reloaded, ["ann@example.com"]); });
  expect(ok).toBe(false);
  expect(view.result.current.drive.driveOutside).toMatchObject({ addresses: ["ann@example.com"], unverified: true, canAttach: false });
});

it("drops a queued batch when compose is reset, so nothing reaches the next draft", async () => {
  const { quill, setError, view } = setup();
  await waitFor(() => expect(view.result.current.drive.driveStatus.available).toBe(true));
  let release!: () => void;
  api.ticket.mockImplementationOnce(() => new Promise((resolve) => {
    release = () => resolve({ uploadUrl: "https://drive.example.com/api/suite/upload/t", expires: "", maxChunk: 64 });
  }));
  act(() => { view.result.current.drive.offerDrive([file("first.bin", 1), file("second.bin", 2)]); });
  let done!: Promise<void>;
  act(() => { done = view.result.current.drive.acceptDriveOffer(); });
  await waitFor(() => expect(api.ticket).toHaveBeenCalledTimes(1));
  act(() => view.result.current.drive.resetDrive());
  release();
  await act(() => done);
  expect(api.ticket).toHaveBeenCalledTimes(1);
  expect(api.link).not.toHaveBeenCalled();
  expect(quill.inserted).toEqual([]);
  expect(setError).not.toHaveBeenCalled();
});

it("leaves the next draft alone when attach-instead or the recipient check finishes after a reset", async () => {
  const { quill, setError, view } = setup();
  await waitFor(() => expect(view.result.current.drive.driveStatus.available).toBe(true));
  act(() => { view.result.current.drive.offerDrive([file("big.bin", 10)]); });
  await act(() => view.result.current.drive.acceptDriveOffer());
  const body = quill.root.innerHTML;

  // Attach instead, with the file read finishing after compose was reset.
  const reader = vi.spyOn(FileReader.prototype, "readAsDataURL");
  let finish!: () => void;
  reader.mockImplementationOnce(function (this: FileReader) {
    finish = () => { Object.defineProperty(this, "result", { value: "data:x;base64,AAAA" }); this.onload?.(new ProgressEvent("load") as ProgressEvent<FileReader>); };
  });
  let attached!: Promise<boolean>;
  act(() => { attached = view.result.current.drive.attachDriveFilesInstead(); });
  act(() => view.result.current.drive.resetDrive());
  finish();
  let result = true;
  await act(async () => { result = await attached; });
  expect(result).toBe(false);
  expect(view.result.current.attachments).toEqual([]);
  expect(quill.root.innerHTML).toBe(body);
  reader.mockRestore();

  // The recipient check answering after a reset holds nothing in the next draft.
  let answer!: (a: string[]) => void;
  api.outside.mockImplementationOnce(() => new Promise((r) => { answer = r; }));
  let ok!: Promise<boolean>;
  act(() => { ok = view.result.current.drive.driveRecipientsOk(body, ["bob@example.net"]); });
  act(() => view.result.current.drive.resetDrive());
  answer(["bob@example.net"]);
  await act(async () => { await ok; });
  expect(view.result.current.drive.driveOutside).toBeNull();
  expect(setError).not.toHaveBeenCalled();
});

// The draft's recipients and subject as the component reads them; tests change it to edit the draft.
let recipients = "dana@partner.example";
const draftKey = () => recipients;

const guestStatus = { available: true, origin: "https://drive.example.com", appUrl: "https://drive.example.com/", guestLinks: true, guestMaxDays: 14 };
const stepUpRequired = () => Object.assign(new HttpError("step up", 403, { code: "step_up_required" }));

/** Uploads big.bin (file f1) and holds a send to an outsider. */
async function heldForOutsider(status: Record<string, unknown> = guestStatus) {
  api.status = status;
  const t = setup();
  await waitFor(() => expect(t.view.result.current.drive.driveStatus.available).toBe(true));
  act(() => { t.view.result.current.drive.offerDrive([file("big.bin", 10)]); });
  await act(() => t.view.result.current.drive.acceptDriveOffer());
  api.outside.mockResolvedValueOnce(["dana@partner.example"]);
  await act(async () => { await t.view.result.current.drive.driveRecipientsOk(t.quill.root.innerHTML, ["dana@partner.example"]); });
  api.status = { available: true, origin: "https://drive.example.com", appUrl: "https://drive.example.com/" };
  return t;
}

it("offers guest links only when KyDrive's status carries them", async () => {
  expect((await heldForOutsider()).view.result.current.drive.driveOutside?.canGuest).toBe(true);
  const old = await heldForOutsider({ available: true, origin: "https://drive.example.com", appUrl: "https://drive.example.com/" });
  expect(old.view.result.current.drive.driveOutside?.canGuest).toBe(false);
});

it("makes guest links at send time and says they expire", async () => {
  const { quill, view } = await heldForOutsider();
  api.link.mockResolvedValueOnce({ url: "https://drive.example.com/g/#guest", expires: "2026-11-21T00:00:00Z" });
  let ok = false;
  await act(async () => { ok = await view.result.current.drive.sendWithGuestLinks(7, undefined, draftKey); });
  expect(ok).toBe(true);
  expect(api.link).toHaveBeenLastCalledWith("f1", 7, "guest");
  const a = quill.root.querySelector("a") as HTMLAnchorElement;
  expect(a.getAttribute("href")).toBe("https://drive.example.com/g/#guest");
  // The fake editor drops insertLink's note, so a new one is added (toGuestLinks's own test covers replacing it).
  expect(a.nextSibling?.textContent).toMatch(/^ \(KyDrive guest link, expires .+\)$/);
  expect(view.result.current.drive.driveOutside).toBeNull();
});

it("holds the send for a step-up, then makes the links once confirmed", async () => {
  const { quill, view } = await heldForOutsider();
  api.link.mockRejectedValueOnce(stepUpRequired());
  let ok = true;
  await act(async () => { ok = await view.result.current.drive.sendWithGuestLinks(7, undefined, draftKey); });
  expect(ok).toBe(false);
  expect(view.result.current.drive.driveGuestStepUp).toMatchObject({ days: 7, sendAt: undefined });
  expect(quill.root.querySelector("a")?.getAttribute("href")).toBe("https://drive.example.com/l/#tok");

  // A failed confirmation (wrong password, or a step-up the server says lapsed) makes nothing.
  await expect(act(() => view.result.current.drive.confirmGuestStepUp(async () => { throw new Error("wrong password"); }, draftKey))).rejects.toThrow("wrong password");
  expect(api.link).toHaveBeenCalledTimes(2);
  api.link.mockResolvedValueOnce({ url: "https://drive.example.com/g/#guest", expires: "" });
  let held: unknown = null;
  await act(async () => { held = await view.result.current.drive.confirmGuestStepUp(async () => {}, draftKey); });
  expect(held).toMatchObject({ days: 7, sendAt: undefined });
  expect(quill.root.querySelector("a")?.getAttribute("href")).toBe("https://drive.example.com/g/#guest");
});

it("never makes guest links for the next draft after compose is reset", async () => {
  // Reset while the step-up is being confirmed.
  const first = await heldForOutsider();
  api.link.mockRejectedValueOnce(stepUpRequired());
  await act(async () => { await first.view.result.current.drive.sendWithGuestLinks(7, undefined, draftKey); });
  const calls = api.link.mock.calls.length;
  let confirmed!: () => void;
  let done!: Promise<unknown>;
  act(() => { done = first.view.result.current.drive.confirmGuestStepUp(() => new Promise((r) => { confirmed = r; }), draftKey); });
  act(() => first.view.result.current.drive.resetDrive());
  confirmed();
  let held: unknown = "unset";
  await act(async () => { held = await done; });
  expect(held).toBeNull();
  expect(api.link).toHaveBeenCalledTimes(calls);
  expect(first.quill.root.querySelector("a")?.getAttribute("href")).toBe("https://drive.example.com/l/#tok");

  // Reset while the first of two links is being made.
  const second = await heldForOutsider();
  second.quill.root.insertAdjacentHTML("beforeend", "<p></p>");
  act(() => { second.view.result.current.drive.offerDrive([file("two.bin", 2)]); });
  api.link.mockResolvedValueOnce({ url: "https://drive.example.com/l/#tok2", expires: "" });
  await act(() => second.view.result.current.drive.acceptDriveOffer());
  let made!: (l: { url: string; expires: string }) => void;
  api.link.mockImplementationOnce(() => new Promise((r) => { made = r; }));
  const before = api.link.mock.calls.length;
  let sending!: Promise<boolean>;
  act(() => { sending = second.view.result.current.drive.sendWithGuestLinks(7, undefined, draftKey); });
  act(() => second.view.result.current.drive.resetDrive());
  made({ url: "https://drive.example.com/g/#late", expires: "" });
  let ok = true;
  await act(async () => { ok = await sending; });
  expect(ok).toBe(false);
  expect(api.link).toHaveBeenCalledTimes(before + 1);
  expect(second.quill.root.innerHTML).not.toContain("/g/");
  expect(second.setError).not.toHaveBeenCalled();
});

it("sends nothing when the draft changes while guest links are made or the step-up runs", async () => {
  // Recipients changed while KyDrive was making the link.
  const { quill, setError, view } = await heldForOutsider();
  let made!: (l: { url: string; expires: string }) => void;
  api.link.mockImplementationOnce(() => new Promise((r) => { made = r; }));
  let sending!: Promise<boolean>;
  act(() => { sending = view.result.current.drive.sendWithGuestLinks(7, undefined, draftKey); });
  recipients = "someone@else.example";
  made({ url: "https://drive.example.com/g/#guest", expires: "" });
  let ok = true;
  await act(async () => { ok = await sending; });
  expect(ok).toBe(false);
  expect(setError).toHaveBeenLastCalledWith(expect.stringMatching(/^The message changed/));
  expect(quill.root.innerHTML).not.toContain("/g/");
  recipients = "dana@partner.example";

  // The body changed instead.
  api.link.mockImplementationOnce(() => new Promise((r) => { made = r; }));
  act(() => { sending = view.result.current.drive.sendWithGuestLinks(7, undefined, draftKey); });
  quill.root.insertAdjacentHTML("beforeend", "<p>a new paragraph</p>");
  made({ url: "https://drive.example.com/g/#guest", expires: "" });
  await act(async () => { ok = await sending; });
  expect(ok).toBe(false);
  expect(quill.root.innerHTML).not.toContain("/g/");

  // Edited between the step-up request and its confirmation: no link is made.
  api.link.mockRejectedValueOnce(Object.assign(new HttpError("step up", 403, { code: "step_up_required" })));
  await act(async () => { await view.result.current.drive.sendWithGuestLinks(7, undefined, draftKey); });
  expect(view.result.current.drive.driveGuestStepUp).not.toBeNull();
  const calls = api.link.mock.calls.length;
  recipients = "someone@else.example";
  let held: unknown = "unset";
  await act(async () => { held = await view.result.current.drive.confirmGuestStepUp(async () => {}, draftKey); });
  expect(held).toBeNull();
  expect(api.link).toHaveBeenCalledTimes(calls);
  expect(quill.root.innerHTML).not.toContain("/g/");
  recipients = "dana@partner.example";
});
