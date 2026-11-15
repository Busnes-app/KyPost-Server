import { afterEach, expect, it, vi } from "vitest";
import { bodyHtml, holdComposeLink, parseComposeLink, takeComposeLink } from "./composeLink";

afterEach(() => {
  vi.useRealTimers();
  window.sessionStorage.clear();
});

const q = (o: Record<string, string>) => "?" + new URLSearchParams(o).toString();

it("reads to, subject and body, dropping anything invalid silently", () => {
  expect(parseComposeLink(q({ to: "ann@example.com, bad, Bob@Example.com,ann@EXAMPLE.com,x<y>@e.com", subject: "Q3 plan", body: "See https://drive.example.com/l/#t\nThanks" })))
    .toEqual({ to: ["ann@example.com", "Bob@Example.com"], subject: "Q3 plan", body: "See https://drive.example.com/l/#t\nThanks" });
  expect(parseComposeLink(q({ to: Array.from({ length: 101 }, (_, i) => `u${i}@example.com`).join(","), subject: "s" }))).toEqual({ to: [], subject: "s", body: "" });
  expect(parseComposeLink(q({ to: "a@example.com", subject: "é".repeat(128) }))).toEqual({ to: ["a@example.com"], subject: "", body: "" });
  expect(parseComposeLink(q({ to: "a@example.com", subject: "a\r\nBcc: x@evil.com" }))?.subject).toBe("");
  expect(parseComposeLink(q({ to: "a@example.com", body: "x".repeat(2001) }))?.body).toBe("");
  expect(parseComposeLink(q({ to: "a@example.com", body: "bell\u0007" }))?.body).toBe("");
  expect(parseComposeLink(q({ body: "x".repeat(2000) }))?.body).toHaveLength(2000);
  expect(parseComposeLink("")).toBeNull();
  expect(parseComposeLink(q({ to: "nope", subject: "\u0000" }))).toBeNull();
});

it("renders the body as escaped text with only http(s) links", () => {
  expect(bodyHtml(`<img src=x onerror=alert(1)> "q" & https://drive.example.com/l/#tok javascript:alert(1)\n\nend`)).toBe(
    `<p>&#60;img src=x onerror=alert(1)&#62; &#34;q&#34; &#38; <a href="https://drive.example.com/l/#tok">https://drive.example.com/l/#tok</a> javascript:alert(1)</p><p><br></p><p>end</p>`
  );
  expect(bodyHtml("https://u:p@evil.example.com/")).toBe("<p>https://u:p@evil.example.com/</p>");
  expect(bodyHtml("")).toBe("");
});

it("keeps the link across sign-in once, for ten minutes, re-checking what it reads back", () => {
  vi.useFakeTimers();
  holdComposeLink(parseComposeLink(q({ to: "ann@example.com", subject: "Hi" })));
  expect(takeComposeLink()).toEqual({ to: ["ann@example.com"], subject: "Hi", body: "" });
  expect(takeComposeLink()).toBeNull();

  holdComposeLink(parseComposeLink(q({ subject: "Late" })));
  vi.advanceTimersByTime(10 * 60_000 + 1);
  expect(takeComposeLink()).toBeNull();

  window.sessionStorage.setItem("kypost.compose-link.v1", JSON.stringify({ to: ["evil"], subject: "a\nb", body: "ok", at: Date.now() }));
  expect(takeComposeLink()).toEqual({ to: [], subject: "", body: "ok" });
  window.sessionStorage.setItem("kypost.compose-link.v1", "{not json");
  expect(takeComposeLink()).toBeNull();
  holdComposeLink(null);
  expect(window.sessionStorage.length).toBe(0);
});
