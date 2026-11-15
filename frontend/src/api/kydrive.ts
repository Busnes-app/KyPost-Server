// KyDrive from KyPost (docs/KYDRIVE.md): status, upload tickets, links and
// saving a received attachment. The browser PUTs file bytes straight to the
// ticket URL on KyDrive; the URL is the credential, so it is never stored,
// logged or put anywhere but that request.

import { getJSON, HttpError, postJSON } from "./client";
import type { ComposeAttachment } from "../app/types";

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

/** A file picked in KyDrive's picker ("Share a link"): an organisation link KyDrive made. */
export type PickedLink = { name: string; size: number; url: string; expires: string };
/** A file picked for "Attach a copy": KyPost fetches it server-side. */
export type PickedCopy = { name: string; size: number; fileId: string };
export type PickMode = "link" | "copy";

const MAX_PICKED = 50;

function pickedName(v: unknown): string {
  if (typeof v !== "string") return "";
  const name = v.replace(/[\u0000-\u001f\u007f]/g, "").trim();
  return name.length <= 255 ? name : "";
}

/**
 * The files of a picker message. Every field is untrusted text from another
 * origin: an entry with a bad name, size, link or ID is dropped.
 */
export function parsePicked(data: unknown, mode: PickMode, origin: string): PickedLink[] | PickedCopy[] | null {
  const msg = data as { type?: unknown; files?: unknown } | null;
  if (typeof msg !== "object" || msg === null || msg.type !== "kydrive.picked" || !Array.isArray(msg.files)) return null;
  const out: (PickedLink | PickedCopy)[] = [];
  for (const raw of msg.files.slice(0, MAX_PICKED)) {
    const f = (typeof raw === "object" && raw !== null ? raw : {}) as Record<string, unknown>;
    const name = pickedName(f.name);
    const size = typeof f.size === "number" && Number.isSafeInteger(f.size) && f.size >= 0 ? f.size : -1;
    if (!name || size < 0) continue;
    if (mode === "link") {
      const url = typeof f.url === "string" && isDriveLink(f.url, origin) ? new URL(f.url).href : "";
      const expires = typeof f.expires === "string" && !Number.isNaN(Date.parse(f.expires)) ? f.expires : "";
      if (url) out.push({ name, size, url, expires });
    } else if (typeof f.file_id === "string" && /^[\x21-\x7e]{1,128}$/.test(f.file_id)) {
      out.push({ name, size, fileId: f.file_id });
    }
  }
  return out as PickedLink[] | PickedCopy[];
}

/**
 * Opens KyDrive's picker (contract I) and resolves with what the user picked,
 * or [] when they close it. Call it straight from a click so the popup is
 * allowed. Only a "kydrive.picked" message from exactly that popup on exactly
 * the paired origin counts; anything else is ignored.
 */
export function pickFromDrive(origin: string, mode: PickMode): Promise<PickedLink[] | PickedCopy[]> {
  const url = new URL("/pick", origin);
  url.searchParams.set("origin", window.location.origin);
  url.searchParams.set("mode", mode);
  const popup = window.open(url.href, "kydrive-picker", "popup,width=760,height=640");
  if (!popup) return Promise.reject(new Error("Allow the KyDrive window to open, then try again."));
  return new Promise((resolve) => {
    let settled = false;
    const done = (files: PickedLink[] | PickedCopy[]) => {
      if (settled) return;
      settled = true;
      window.removeEventListener("message", onMessage);
      window.clearInterval(timer);
      resolve(files);
    };
    const onMessage = (e: MessageEvent) => {
      if (e.origin !== origin || e.source !== popup) return;
      const files = parsePicked(e.data, mode, origin);
      if (files) done(files);
    };
    window.addEventListener("message", onMessage);
    // The picker posts, then closes: give a message already on its way a moment.
    const timer = window.setInterval(() => { if (popup.closed) window.setTimeout(() => done([]), 500); }, 500);
  });
}

/** Fetches a picked file through KyPost (contract I2) as a compose attachment. */
export async function copyFromDrive(fileId: string): Promise<ComposeAttachment> {
  const a = await postJSON<Record<string, unknown>>("/api/kydrive/copies", { fileId });
  if (typeof a.name !== "string" || typeof a.dataBase64 !== "string" || typeof a.size !== "number") throw new Error("KyDrive gave an unusable file.");
  return { name: a.name, mimeType: typeof a.mimeType === "string" ? a.mimeType : "application/octet-stream", dataBase64: a.dataBase64, size: a.size };
}
