import { afterEach, expect, it, vi } from "vitest";
import { driveLinksIn, guestDayChoices, isDriveLink, isGuestLink, loadDriveStatus, toGuestLinks, uploadToTicket } from "./kydrive";

afterEach(() => vi.unstubAllGlobals());

const origin = "https://drive.example.com";

it("finds only KyDrive links on the paired origin", () => {
  expect(isDriveLink(`${origin}/l/#tok`, origin)).toBe(true);
  for (const href of [`${origin}/l/`, `${origin}/x/#tok`, "https://evil.example.com/l/#tok", `http://drive.example.com/l/#tok`, "javascript:alert(1)"]) {
    expect(isDriveLink(href, origin)).toBe(false);
  }
  expect(isDriveLink(`${origin}/l/#tok`, "")).toBe(false);
  const html = `<p><a href="${origin}/l/#a">a.pdf</a> (1 MB)</p><p><a href="https://other.example.com/l/#b">b</a></p>`;
  expect(driveLinksIn(html, origin)).toEqual([`${origin}/l/#a`]);
});

it("reads the status strictly", async () => {
  const answer = (body: unknown) => vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } })));
  answer({ available: true, appUrl: "https://drive.example.com/", origin, maxFileBytes: 5 });
  expect(await loadDriveStatus()).toEqual({ available: true, appUrl: "https://drive.example.com/", origin, maxFileBytes: 5, freeBytes: undefined });
  answer({ available: true, appUrl: "javascript:alert(1)", origin });
  expect(await loadDriveStatus()).toEqual({ available: false, origin });
  answer({ available: false, origin: "http://drive.example.com" });
  expect(await loadDriveStatus()).toEqual({ available: false });
  vi.stubGlobal("fetch", vi.fn(async () => { throw new Error("down"); }));
  expect(await loadDriveStatus()).toEqual({ available: false });
});

type Sent = { method: string; url: string; headers: Record<string, string>; size: number; credentials: boolean };

function fakeXHR(sent: Sent[], status: (i: number) => number = (i) => (i === 2 ? 201 : 202)) {
  return class {
    upload: { onprogress?: (e: { loaded: number }) => void } = {};
    withCredentials = true;
    status = 0;
    responseText = "";
    onload?: () => void;
    onabort?: () => void;
    onerror?: () => void;
    private s: Sent = { method: "", url: "", headers: {}, size: 0, credentials: true };
    open(method: string, url: string) { this.s.method = method; this.s.url = url; }
    setRequestHeader(k: string, v: string) { this.s.headers[k] = v; }
    abort() { this.onabort?.(); }
    send(body: Blob) {
      this.s.size = body.size;
      this.s.credentials = this.withCredentials;
      sent.push(this.s);
      const i = sent.length - 1;
      this.upload.onprogress?.({ loaded: body.size });
      this.status = status(i);
      this.responseText = this.status === 201 ? JSON.stringify({ file_id: "f1", name: "a (2).bin", size: 10 }) : "";
      queueMicrotask(() => this.onload?.());
    }
  };
}

it("PUTs the file in ordered Content-Range chunks without credentials", async () => {
  const sent: Sent[] = [];
  vi.stubGlobal("XMLHttpRequest", fakeXHR(sent));
  const progress: number[] = [];
  const url = `${origin}/api/suite/upload/tok`;
  const file = await uploadToTicket({ uploadUrl: url, expires: "", maxChunk: 4 }, new Blob(["0123456789"]), (n) => progress.push(n), new AbortController().signal);
  expect(file).toEqual({ file_id: "f1", name: "a (2).bin", size: 10 });
  expect(sent.map((s) => [s.method, s.url, s.headers["Content-Range"], s.size, s.credentials])).toEqual([
    ["PUT", url, "bytes 0-3/10", 4, false],
    ["PUT", url, "bytes 4-7/10", 4, false],
    ["PUT", url, "bytes 8-9/10", 2, false]
  ]);
  expect(progress).toEqual([4, 8, 10]);
  expect(sent.some((s) => "X-CSRF-Token" in s.headers)).toBe(false);
});

it("sends one PUT without Content-Range when the file fits a chunk, and stops on a refusal", async () => {
  const sent: Sent[] = [];
  vi.stubGlobal("XMLHttpRequest", fakeXHR(sent, () => 201));
  await uploadToTicket({ uploadUrl: `${origin}/api/suite/upload/t`, expires: "", maxChunk: 64 }, new Blob(["abc"]), () => {}, new AbortController().signal);
  expect(sent).toHaveLength(1);
  expect(sent[0].headers["Content-Range"]).toBeUndefined();
  const refused: Sent[] = [];
  vi.stubGlobal("XMLHttpRequest", fakeXHR(refused, () => 409));
  await expect(uploadToTicket({ uploadUrl: `${origin}/api/suite/upload/t`, expires: "", maxChunk: 1 }, new Blob(["abc"]), () => {}, new AbortController().signal)).rejects.toThrow(/409/);
  expect(refused).toHaveLength(1);
});

it("cancels before sending once aborted", async () => {
  const sent: Sent[] = [];
  vi.stubGlobal("XMLHttpRequest", fakeXHR(sent));
  const abort = new AbortController();
  abort.abort();
  await expect(uploadToTicket({ uploadUrl: `${origin}/api/suite/upload/t`, expires: "", maxChunk: 4 }, new Blob(["abc"]), () => {}, abort.signal)).rejects.toThrow(/cancelled/);
  expect(sent).toHaveLength(0);
});

it("reads guest links from the status only when they are well formed", async () => {
  const answer = (body: unknown) => vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } })));
  const base = { available: true, appUrl: "https://drive.example.com/", origin };
  answer({ ...base, guestLinks: true, guestMaxDays: 14 });
  expect(await loadDriveStatus()).toMatchObject({ guestLinks: true, guestMaxDays: 14 });
  for (const odd of [{}, { guestLinks: true }, { guestLinks: "true", guestMaxDays: 7 }, { guestLinks: true, guestMaxDays: 0 }, { guestLinks: true, guestMaxDays: 1.5 }, { guestLinks: true, guestMaxDays: 9999 }]) {
    answer({ ...base, ...odd });
    const s = await loadDriveStatus();
    expect(s.available).toBe(true);
    expect(s.guestLinks).toBeUndefined();
  }
});

it("rewrites organisation links into guest links with their expiry", () => {
  expect(isGuestLink(`${origin}/g/#t`, origin)).toBe(true);
  expect(isGuestLink(`${origin}/l/#t`, origin)).toBe(false);
  expect(guestDayChoices(14)).toEqual([1, 7, 14]);
  expect(guestDayChoices(1)).toEqual([1]);
  expect(guestDayChoices(30)).toEqual([1, 7, 14, 30]);
  const root = document.createElement("div");
  root.innerHTML = `<p><a href="${origin}/l/#a">a.pdf</a> (2 MB, KyDrive, link expires 1/1/2027)</p><p><a href="${origin}/l/#b">b.pdf</a> edited</p><p><a href="${origin}/l/#c">c</a></p>`;
  toGuestLinks(root, new Map([[`${origin}/l/#a`, { url: `${origin}/g/#ga`, expires: "" }], [`${origin}/l/#b`, { url: `${origin}/g/#gb`, expires: "" }]]));
  expect(root.innerHTML).toBe(`<p><a href="${origin}/g/#ga">a.pdf</a> (2 MB, KyDrive guest link, expires)</p><p><a href="${origin}/g/#gb">b.pdf</a> (KyDrive guest link, expires) edited</p><p><a href="${origin}/l/#c">c</a></p>`);
});
