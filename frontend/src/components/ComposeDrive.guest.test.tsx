// Guest links through the real ComposeDrive and useComposeDrive (contract K): the
// send that follows a step-up uses the compose as it is then, and Cancel stops it.
import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import type Quill from "quill";
import { HttpError } from "../api/client";
import type { Compose } from "../hooks/useCompose";
import type { ComposeAttachment } from "../app/types";

const api = vi.hoisted(() => ({
  link: vi.fn(async (_id: string, _days?: number, audience?: string) => (audience
    ? { url: "https://drive.example.com/g/#guest", expires: "" }
    : { url: "https://drive.example.com/l/#tok", expires: "" })),
  reauth: vi.fn(async (_p: string, _c: string) => {})
}));
vi.mock("../api/kydrive", async (importActual) => ({
  ...(await importActual<typeof import("../api/kydrive")>()),
  loadDriveStatus: async () => ({ available: true, origin: "https://drive.example.com", appUrl: "https://drive.example.com/", guestLinks: true, guestMaxDays: 30 }),
  requestUploadTicket: async () => ({ uploadUrl: "https://drive.example.com/api/suite/upload/t", expires: "", maxChunk: 64 }),
  uploadToTicket: async (_t: unknown, blob: Blob) => ({ file_id: "f1", name: "big.bin", size: blob.size }),
  createDriveLink: api.link,
  checkOutsideRecipients: async () => ["dana@partner.example"]
}));
vi.mock("../api/auth", () => ({ reauthenticate: api.reauth, reauthenticateWithSSO: vi.fn() }));

import { useComposeDrive } from "../hooks/useComposeDrive";
import { ComposeDrive } from "./ComposeDrive";

afterEach(() => { cleanup(); vi.clearAllMocks(); });

const root = document.createElement("div");
const quill = {
  root,
  getSelection: () => null,
  getLength: () => 1,
  insertText: (_at: number, text: string, formats: { link?: unknown }) => {
    if (typeof formats.link === "string") root.insertAdjacentHTML("beforeend", `<p><a href="${formats.link}">${text}</a></p>`);
  },
  update: vi.fn()
};
const send = vi.fn(async (_at?: Date, _skip?: boolean) => {});
let setTo!: (to: string) => void;
let drive!: ReturnType<typeof useComposeDrive>;

function Harness() {
  const [to, setToState] = useState("dana@partner.example");
  const [attachments, setAttachments] = useState<ComposeAttachment[]>([]);
  setTo = setToState;
  drive = useComposeDrive({ composeOpen: true, quill: { current: quill as unknown as Quill }, attachments, setAttachments, setError: vi.fn() });
  const field = (email: string) => ({ tokens: email ? [{ email, isCustom: true }] : [], draft: "" });
  const compose = { drive, composeSending: false, sendComposeEmail: (at?: Date, skip?: boolean) => send(at, skip),
    composeTo: field(to), composeCc: field(""), composeBcc: field(""), composeSubject: "Survey" } as unknown as Compose;
  return <ComposeDrive compose={compose} />;
}

/** Uploads a file, holds the send for the outsider, and chooses guest links; the server wants a step-up. */
async function atStepUp() {
  root.innerHTML = "";
  render(<Harness />);
  await waitFor(() => expect(drive.driveStatus.available).toBe(true));
  act(() => { drive.offerDrive([new File([new Uint8Array(4)], "big.bin")]); });
  await act(() => drive.acceptDriveOffer());
  await act(async () => { await drive.driveRecipientsOk(root.innerHTML, ["dana@partner.example"]); });
  api.link.mockRejectedValueOnce(new HttpError("step up", 403, { code: "step_up_required" }));
  fireEvent.click(screen.getByRole("button", { name: "Send a guest link instead" }));
  await screen.findByText(/confirm it is you before KyPost makes them/);
  fireEvent.change(document.querySelector('.compose-drive-stepup input[type="password"]') as HTMLInputElement, { target: { value: "pw" } });
}

it("sends nothing when a recipient changes while the guest links are made after the step-up", async () => {
  await atStepUp();
  let made!: (l: { url: string; expires: string }) => void;
  api.link.mockImplementationOnce(() => new Promise((r) => { made = r; }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm and send" }));
  await waitFor(() => expect(api.link).toHaveBeenLastCalledWith("f1", 7, "guest"));
  expect(screen.queryByText(/confirm it is you before/)).toBeNull(); // the form has unmounted
  act(() => setTo("someone@else.example"));
  await act(async () => { made({ url: "https://drive.example.com/g/#guest", expires: "" }); });
  expect(send).not.toHaveBeenCalled();
  expect(root.innerHTML).not.toContain("/g/");
});

it("sends nothing after Cancel, even when the confirmation then succeeds", async () => {
  await atStepUp();
  let confirmed!: () => void;
  api.reauth.mockImplementationOnce(() => new Promise((r) => { confirmed = () => r(); }));
  fireEvent.click(screen.getByRole("button", { name: "Confirm and send" }));
  fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
  const calls = api.link.mock.calls.length;
  await act(async () => { confirmed(); });
  expect(api.link).toHaveBeenCalledTimes(calls);
  expect(send).not.toHaveBeenCalled();
  expect(root.innerHTML).not.toContain("/g/");
});

it("makes the links and sends when nothing changed", async () => {
  await atStepUp();
  fireEvent.click(screen.getByRole("button", { name: "Confirm and send" }));
  await waitFor(() => expect(send).toHaveBeenCalledWith(undefined, true));
  expect(root.innerHTML).toContain("/g/#guest");
});
