import { useEffect, useRef, useState, type Dispatch, type RefObject, type SetStateAction } from "react";
import type Quill from "quill";
import { toErrorMessage } from "../api/client";
import {
  checkOutsideRecipients,
  createDriveLink,
  driveErrorCode,
  driveLinksIn,
  loadDriveStatus,
  requestUploadTicket,
  uploadToTicket,
  type DriveStatus
} from "../api/kydrive";
import { formatBytes, MAX_ATTACHMENT_BYTES, readFileAsAttachment } from "../app/compose";
import type { ComposeAttachment } from "../app/types";

export type DriveUpload = { id: number; name: string; loaded: number; size: number };
/** Recipients who cannot open the message's KyDrive links. unverified: the check itself failed. */
export type DriveOutside = { addresses: string[]; unverified: boolean; canAttach: boolean; sendAt?: Date };

/** KyDrive in compose (docs/KYDRIVE.md "KyPost side"): offer, upload, link, and the send-time check. */
export function useComposeDrive({
  composeOpen, quill, attachments, setAttachments, setError
}: {
  composeOpen: boolean;
  quill: RefObject<Quill | null>;
  attachments: ComposeAttachment[];
  setAttachments: Dispatch<SetStateAction<ComposeAttachment[]>>;
  setError: (message: string) => void;
}) {
  const [driveStatus, setDriveStatus] = useState<DriveStatus>({ available: false });
  const [driveOffer, setDriveOffer] = useState<File[]>([]);
  const [driveUploads, setDriveUploads] = useState<DriveUpload[]>([]);
  const [driveOutside, setDriveOutside] = useState<DriveOutside | null>(null);
  const aborts = useRef(new Map<number, AbortController>());
  // Link URL -> the file behind it, for "attach instead"; this window only.
  const files = useRef(new Map<string, File>());
  const seq = useRef(0);
  // Bumped by resetDrive: work started for an earlier compose never touches the next one.
  const generation = useRef(0);

  useEffect(() => {
    if (!composeOpen) return;
    let live = true;
    void loadDriveStatus().then((s) => { if (live) setDriveStatus(s); });
    return () => { live = false; };
  }, [composeOpen]);

  function resetDrive() {
    generation.current += 1;
    for (const c of aborts.current.values()) c.abort();
    aborts.current.clear();
    files.current.clear();
    setDriveOffer([]);
    setDriveUploads([]);
    setDriveOutside(null);
  }

  /** Inserts "name" linked to url, then size and expiry as text, at the cursor. */
  function insertLink(name: string, url: string, size: number, expires: string) {
    const q = quill.current;
    if (!q) return;
    const at = q.getSelection()?.index ?? Math.max(0, q.getLength() - 1);
    const until = expires ? `link expires ${new Date(expires).toLocaleDateString()}` : "link does not expire";
    q.insertText(at, name, { link: url }, "user");
    q.insertText(at + name.length, ` (${formatBytes(size)}, KyDrive, ${until})\n`, { link: false }, "user");
  }

  /** Uploads one file and links it. A failure leaves the draft as it was. */
  async function uploadToDrive(file: File, gen = generation.current): Promise<boolean> {
    if (gen !== generation.current) return false;
    const id = ++seq.current;
    const abort = new AbortController();
    aborts.current.set(id, abort);
    setDriveUploads((u) => [...u, { id, name: file.name, loaded: 0, size: file.size }]);
    try {
      const stale = () => abort.signal.aborted || gen !== generation.current;
      const ticket = await requestUploadTicket(file.name, file.size);
      if (stale()) return false;
      const saved = await uploadToTicket(ticket, file, (loaded) =>
        setDriveUploads((u) => u.map((x) => (x.id === id ? { ...x, loaded } : x))), abort.signal);
      if (stale()) return false;
      const link = await createDriveLink(saved.file_id);
      if (stale()) return false;
      insertLink(saved.name || file.name, link.url, file.size, link.expires);
      files.current.set(link.url, file);
      return true;
    } catch (e) {
      if (!abort.signal.aborted && gen === generation.current) setError(`Upload to KyDrive failed: ${driveMessage(e)}`);
      return false;
    } finally {
      aborts.current.delete(id);
      setDriveUploads((u) => u.filter((x) => x.id !== id));
    }
  }

  function cancelDriveUpload(id: number) {
    aborts.current.get(id)?.abort();
  }

  /** Picked files over the attachment limit: offer KyDrive when it is there. */
  function offerDrive(picked: File[]): boolean {
    if (!driveStatus.available) return false;
    setDriveOffer(picked);
    return true;
  }

  async function acceptDriveOffer() {
    const picked = driveOffer;
    const gen = generation.current;
    setDriveOffer([]);
    for (const f of picked) {
      if (gen !== generation.current) return;
      await uploadToDrive(f, gen);
    }
  }

  /** Moves an existing attachment to KyDrive; it stays attached if that fails. */
  async function attachmentToDrive(index: number) {
    const a = attachments[index];
    if (!a) return;
    const gen = generation.current;
    const bytes = Uint8Array.from(atob(a.dataBase64), (c) => c.charCodeAt(0));
    if (await uploadToDrive(new File([bytes], a.name, { type: a.mimeType }), gen)) {
      setAttachments((current) => current.filter((x) => x !== a));
    }
  }

  /**
   * Run before a send: if the body carries KyDrive links and a recipient is
   * outside the organisation (or the check fails), holds the send and says who.
   * Returns true when the send may go ahead.
   */
  async function driveRecipientsOk(body: string, recipients: string[], sendAt?: Date): Promise<boolean> {
    const links = driveLinksIn(body, driveStatus.origin ?? "");
    if (links.length === 0 || recipients.length === 0) return true;
    let outside: string[];
    let unverified = false;
    try {
      outside = await checkOutsideRecipients(recipients);
    } catch {
      outside = recipients;
      unverified = true;
    }
    if (outside.length === 0) return true;
    const held = links.map((l) => files.current.get(l));
    const total = attachments.reduce((s, a) => s + a.size, 0) + held.reduce((s, f) => s + (f?.size ?? 0), 0);
    setDriveOutside({ addresses: outside, unverified, sendAt, canAttach: held.every(Boolean) && total <= MAX_ATTACHMENT_BYTES });
    return false;
  }

  /** "Attach instead": the held files become attachments and their link lines go. */
  async function attachDriveFilesInstead(): Promise<boolean> {
    const q = quill.current;
    if (!q) return false;
    try {
      const anchors = Array.from(q.root.querySelectorAll<HTMLAnchorElement>("a[href]")).filter((a) => files.current.has(a.getAttribute("href") ?? ""));
      const picked = await Promise.all(anchors.map((a) => readFileAsAttachment(files.current.get(a.getAttribute("href") ?? "") as File)));
      for (const a of anchors) (a.closest("p") ?? a).remove();
      q.update("user");
      setAttachments((current) => [...current, ...picked]);
      setDriveOutside(null);
      return true;
    } catch (e) {
      setError(toErrorMessage(e, "failed to attach the files"));
      return false;
    }
  }

  return {
    driveStatus, driveOffer, setDriveOffer, acceptDriveOffer, driveUploads, cancelDriveUpload, offerDrive,
    attachmentToDrive, driveOutside, setDriveOutside, driveRecipientsOk, attachDriveFilesInstead, resetDrive
  };
}

const driveMessages: Record<string, string> = {
  too_large: "the file is larger than KyDrive accepts.",
  quota: "your KyDrive is full.",
  too_many_tickets: "too many uploads at once; wait for one to finish.",
  sharing_off: "KyPost is not allowed to save to your KyDrive. Turn it on in KyDrive settings.",
  kydrive_off: "KyDrive is not available for your account.",
  bad_body: "KyDrive refused that file name."
};

function driveMessage(e: unknown): string {
  return driveMessages[driveErrorCode(e)] ?? toErrorMessage(e, "KyDrive could not be reached.");
}
