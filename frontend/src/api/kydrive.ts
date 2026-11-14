// KyDrive from KyPost (docs/KYDRIVE.md): status, upload tickets, links and
// saving a received attachment. The browser PUTs file bytes straight to the
// ticket URL on KyDrive; the URL is the credential, so it is never stored,
// logged or put anywhere but that request.

import { getJSON, HttpError, postJSON } from "./client";

export type DriveStatus = { available: boolean; freeBytes?: number; maxFileBytes?: number; appUrl?: string; origin?: string };

export type DriveTicket = { uploadUrl: string; expires: string; maxChunk: number };
export type DriveFile = { file_id: string; name: string; size: number };
export type DriveLink = { url: string; expires: string };
export type DriveSaved = { fileId: string; name: string; size: number; folder: string; appUrl?: string };

const off: DriveStatus = { available: false };

function httpsOrigin(raw: unknown): string {
  if (typeof raw !== "string") return "";
  try {
    const u = new URL(raw);
    return u.protocol === "https:" && !u.username && !u.password ? u.origin : "";
  } catch {
    return "";
  }
}

/** The caller's KyDrive status; any failure or odd answer reads as unavailable. */
export async function loadDriveStatus(): Promise<DriveStatus> {
  try {
    const s = await getJSON<Record<string, unknown>>("/api/kydrive/status");
    const origin = httpsOrigin(s.origin);
    const appUrl = httpsOrigin(s.appUrl) ? String(s.appUrl) : undefined;
    if (s.available !== true || !appUrl) return origin ? { ...off, origin } : off;
    return {
      available: true,
      freeBytes: typeof s.freeBytes === "number" ? s.freeBytes : undefined,
      maxFileBytes: typeof s.maxFileBytes === "number" ? s.maxFileBytes : undefined,
      appUrl,
      origin
    };
  } catch {
    return off;
  }
}

export function requestUploadTicket(name: string, size: number): Promise<DriveTicket> {
  return postJSON<DriveTicket>("/api/kydrive/upload-tickets", { name, size });
}

export function createDriveLink(fileId: string, days = 30): Promise<DriveLink> {
  return postJSON<DriveLink>("/api/kydrive/links", { fileId, days });
}

export function saveAttachmentToDrive(messageId: string, mailbox: string, index: number): Promise<DriveSaved> {
  return postJSON<DriveSaved>("/api/kydrive/save-attachment", { messageId, mailbox, index });
}

/** The KyDrive refusal code of an error, if it is one. */
export function driveErrorCode(error: unknown): string {
  const body = error instanceof HttpError ? (error.body as { code?: unknown } | undefined) : undefined;
  return typeof body?.code === "string" ? body.code : "";
}

/**
 * PUTs blob to the ticket in order, in chunks of at most maxChunk with
 * Content-Range when larger. No cookies, no CSRF header: the ticket is the
 * credential. Aborting signal aborts the request in flight.
 */
export async function uploadToTicket(ticket: DriveTicket, blob: Blob, onProgress: (loaded: number) => void, signal: AbortSignal): Promise<DriveFile> {
  const size = blob.size;
  const chunk = ticket.maxChunk > 0 ? Math.min(ticket.maxChunk, Math.max(size, 1)) : Math.max(size, 1);
  for (let offset = 0; ; offset += chunk) {
    const end = Math.min(offset + chunk, size);
    const range = size > chunk ? `bytes ${offset}-${end - 1}/${size}` : "";
    const answer = await putChunk(ticket.uploadUrl, blob.slice(offset, end), range, (n) => onProgress(offset + n), signal);
    if (end >= size) {
      const file = answer as Partial<DriveFile> | undefined;
      if (typeof file?.file_id !== "string" || !file.file_id) throw new Error("KyDrive did not confirm the upload.");
      return { file_id: file.file_id, name: typeof file.name === "string" ? file.name : "", size: typeof file.size === "number" ? file.size : size };
    }
  }
}

function putChunk(url: string, body: Blob, range: string, onProgress: (loaded: number) => void, signal: AbortSignal): Promise<unknown> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) {
      reject(new DOMException("upload cancelled", "AbortError"));
      return;
    }
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", url);
    xhr.withCredentials = false;
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    if (range) xhr.setRequestHeader("Content-Range", range);
    xhr.upload.onprogress = (e) => onProgress(e.loaded);
    xhr.onerror = () => reject(new Error("The upload to KyDrive failed; check your connection and try again."));
    xhr.onabort = () => reject(new DOMException("upload cancelled", "AbortError"));
    xhr.onload = () => {
      let data: unknown;
      try { data = JSON.parse(xhr.responseText); } catch { data = undefined; }
      if (xhr.status < 200 || xhr.status >= 300) {
        reject(new HttpError(`KyDrive refused the upload: ${xhr.status}`, xhr.status, data));
        return;
      }
      resolve(data);
    };
    signal.addEventListener("abort", () => xhr.abort(), { once: true });
    xhr.send(body);
  });
}

/** Whether href is a KyDrive link (origin/l/#token) on the paired origin. */
export function isDriveLink(href: string, origin: string): boolean {
  if (!origin) return false;
  try {
    const u = new URL(href);
    return u.origin === origin && u.pathname === "/l/" && u.hash.length > 1;
  } catch {
    return false;
  }
}

/** The KyDrive links in a compose body's HTML, in order. */
export function driveLinksIn(html: string, origin: string): string[] {
  if (!origin || !html) return [];
  const doc = new DOMParser().parseFromString(html, "text/html");
  return Array.from(doc.querySelectorAll("a[href]"))
    .map((a) => a.getAttribute("href") ?? "")
    .filter((href) => isDriveLink(href, origin));
}

/** Recipients who are not people in the organisation, so cannot open a KyDrive link. */
export async function checkOutsideRecipients(addresses: string[]): Promise<string[]> {
  const answer = await postJSON<{ outside?: unknown }>("/api/kydrive/recipients", { addresses });
  if (!Array.isArray(answer.outside)) throw new Error("bad answer");
  return answer.outside.filter((a): a is string => typeof a === "string");
}
