import { afterEach, expect, it, vi } from "vitest";
import { act, renderHook, waitFor } from "@testing-library/react";
import { useState } from "react";
import type Quill from "quill";
import type { ComposeAttachment } from "../app/types";

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
  expect(view.result.current.drive.driveOutside).toEqual({ addresses: ["bob@example.net"], unverified: false, canAttach: true, sendAt: undefined });

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
