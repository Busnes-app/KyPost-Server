import { useEffect, useRef, useState, type Dispatch, type RefObject, type SetStateAction } from "react";
import type Quill from "quill";
import { toErrorMessage } from "../api/client";
import {
  checkOutsideRecipients,
  copyFromDrive,
  createDriveLink,
  driveErrorCode,
  driveLinksIn,
  loadDriveStatus,
  pickFromDrive,
  requestUploadTicket,
  uploadToTicket,
  type DriveStatus,
  type PickedCopy,
  type PickedLink,
  type PickMode
} from "../api/kydrive";
import { formatBytes, MAX_ATTACHMENT_BYTES, readFileAsAttachment } from "../app/compose";
import type { ComposeAttachment } from "../app/types";

/** An upload to KyDrive, or (loaded undefined) a copy being fetched from it. */
export type DriveUpload = { id: number; name: string; loaded?: number; size: number };
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

  /**
   * "Attach from KyDrive": opens the picker (call from the click), then links
   * the picked files or fetches copies of them as attachments. A compose
   * reset while the picker or a fetch is open drops the result.
   */
  async function attachFromDrive(mode: PickMode) {
    const origin = driveStatus.origin ?? "";
    if (!driveStatus.available || !origin) return;
    const gen = generation.current;
    let picked: PickedLink[] | PickedCopy[];
    try {
      picked = await pickFromDrive(origin, mode);
    } catch (e) {
      setError(toErrorMessage(e, "KyDrive could not be opened."));
      return;
    }
    if (gen !== generation.current) return;
    if (mode === "link") {
      for (const f of picked as PickedLink[]) insertLink(f.name, f.url, f.size, f.expires);
      return;
    }
    let total = attachments.reduce((sum, a) => sum + a.size, 0);
    for (const f of picked as PickedCopy[]) {
      if (gen !== generation.current) return;
      if (total + f.size > MAX_ATTACHMENT_BYTES) {
        setError(`${f.name} does not fit the ${formatBytes(MAX_ATTACHMENT_BYTES)} attachment limit; share a link instead.`);
        continue;
      }
      const id = ++seq.current;
      const abort = new AbortController();
      aborts.current.set(id, abort);
      setDriveUploads((u) => [...u, { id, name: f.name, size: f.size }]);
      try {
        const a = await copyFromDrive(f.fileId);
        if (abort.signal.aborted || gen !== generation.current) continue;
        total += a.size;
        setAttachments((current) => [...current, a]);
      } catch (e) {
        if (!abort.signal.aborted && gen === generation.current) setError(`Copy from KyDrive failed: ${driveErrorCode(e) === "too_large" ? "the file is over the attachment limit; share a link instead." : driveMessage(e)}`);
      } finally {
        aborts.current.delete(id);
        setDriveUploads((u) => u.filter((x) => x.id !== id));
      }
    }
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
    const gen = generation.current;
    let outside: string[];
    let unverified = false;
    try {
      outside = await checkOutsideRecipients(recipients);
    } catch {
      outside = recipients;
      unverified = true;
    }
    if (gen !== generation.current) return false;
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
    const gen = generation.current;
    try {
      const anchors = Array.from(q.root.querySelectorAll<HTMLAnchorElement>("a[href]")).filter((a) => files.current.has(a.getAttribute("href") ?? ""));
      const picked = await Promise.all(anchors.map((a) => readFileAsAttachment(files.current.get(a.getAttribute("href") ?? "") as File)));
      if (gen !== generation.current) return false;
      for (const a of anchors) (a.closest("p") ?? a).remove();
      q.update("user");
      setAttachments((current) => [...current, ...picked]);
      setDriveOutside(null);
      return true;
    } catch (e) {
      if (gen === generation.current) setError(toErrorMessage(e, "failed to attach the files"));
      return false;
    }
  }

  return {
    driveStatus, driveOffer, setDriveOffer, acceptDriveOffer, attachFromDrive, driveUploads, cancelDriveUpload, offerDrive,
    attachmentToDrive, driveOutside, setDriveOutside, driveRecipientsOk, attachDriveFilesInstead, resetDrive
  };
}

const driveMessages: Record<string, string> = {
  too_large: "the file is larger than KyDrive accepts.",
  quota: "your KyDrive is full.",
  too_many_tickets: "too many uploads at once; wait for one to finish.",
  sharing_off: "KyPost is not allowed to save to your KyDrive. Turn it on in KyDrive settings.",
  kydrive_off: "KyDrive is not available for your account.",
  bad_body: "KyDrive refused that file name.",
  not_found: "that file is no longer in your KyDrive, or you cannot read it."
};

function driveMessage(e: unknown): string {
  return driveMessages[driveErrorCode(e)] ?? toErrorMessage(e, "KyDrive could not be reached.");
}
