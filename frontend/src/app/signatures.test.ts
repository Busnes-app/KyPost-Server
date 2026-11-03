import { describe, expect, it } from "vitest";
import type { SignatureSettings } from "../api/signatures";
import { insertSignature, signatureBlock, signatureTextFor, swapSignature } from "./signatures";

const settings: SignatureSettings = {
  signatures: [
    { id: "work", name: "Work", text: "Ada\nAnalyst" },
    { id: "short", name: "Short", text: "A." }
  ],
  defaultId: "work",
  byAddress: { "alias@example.org": "short", "quiet@example.org": "" },
  onReply: true,
  onForward: false
};

describe("signatureTextFor", () => {
  it("uses the From address's own signature, case-insensitively, else the default", () => {
    expect(signatureTextFor(settings, "Alias@Example.org", "new")).toBe("A.");
    expect(signatureTextFor(settings, "other@example.org", "new")).toBe("Ada\nAnalyst");
    expect(signatureTextFor(settings, "", "new")).toBe("Ada\nAnalyst");
  });

  it("treats an empty per-address entry as 'none' rather than falling back", () => {
    expect(signatureTextFor(settings, "quiet@example.org", "new")).toBe("");
  });

  it("follows the reply and forward switches", () => {
    expect(signatureTextFor(settings, "", "reply")).toBe("Ada\nAnalyst");
    expect(signatureTextFor(settings, "", "forward")).toBe("");
  });

  it("gives nothing without settings, a default, or a signature that exists", () => {
    expect(signatureTextFor(null, "", "new")).toBe("");
    expect(signatureTextFor({ ...settings, defaultId: "" }, "", "new")).toBe("");
    expect(signatureTextFor({ ...settings, defaultId: "gone" }, "", "new")).toBe("");
    // An inherited key is not a per-address entry.
    expect(signatureTextFor({ ...settings, byAddress: {} }, "constructor", "new")).toBe("Ada\nAnalyst");
  });
});

describe("signatureBlock", () => {
  it("escapes markup so a signature can never become live HTML", () => {
    const block = signatureBlock('<img src=x onerror="alert(1)"> & co');
    const doc = new DOMParser().parseFromString(block, "text/html");
    expect(doc.querySelector("img")).toBeNull();
    expect(doc.body.textContent).toBe('<img src=x onerror="alert(1)"> & co');
  });

  it("is one paragraph per line, after a blank separator, in the form the editor writes back", () => {
    const block = signatureBlock("Ada\n\nA & B <x>");
    expect(block).toBe("<p><br></p><p>Ada</p><p><br></p><p>A &amp; B &lt;x&gt;</p>");
    const div = document.createElement("div");
    div.innerHTML = block;
    expect(div.innerHTML).toBe(block);
  });

  it("is empty for an empty signature", () => {
    expect(signatureBlock("  \n ")).toBe("");
  });
});

describe("insertSignature", () => {
  const block = signatureBlock("Ada");

  it("puts a new message's signature below an empty first line", () => {
    expect(insertSignature("", block)).toBe(`<p><br></p>${block}`);
  });

  it("puts a reply's signature above the quoted message", () => {
    const reply = "<p><br /></p><p>On Monday, Bob wrote:</p><blockquote>hi</blockquote>";
    expect(insertSignature(reply, block)).toBe(`<p><br /></p>${block}<p><br></p><p>On Monday, Bob wrote:</p><blockquote>hi</blockquote>`);
  });

  it("appends after text typed before the signature arrived", () => {
    expect(insertSignature("<p>Hello</p>", block)).toBe(`<p>Hello</p>${block}`);
  });

  it("leaves the body alone with no signature", () => {
    expect(insertSignature("<p>Hello</p>", "")).toBe("<p>Hello</p>");
  });
});

describe("swapSignature", () => {
  const a = signatureBlock("Ada");
  const b = signatureBlock("Bob");

  it("replaces the inserted signature when From changes", () => {
    expect(swapSignature(`<p>Hi</p>${a}`, a, b)).toBe(`<p>Hi</p>${b}`);
  });

  it("removes it when the new address has none, and inserts one when the old had none", () => {
    expect(swapSignature(`<p>Hi</p>${a}`, a, "")).toBe("<p>Hi</p>");
    expect(swapSignature("<p><br></p>", "", b)).toBe(`<p><br></p>${b}`);
  });

  it("leaves a signature the user edited alone", () => {
    const edited = "<p>Hi</p><p><br></p><p>Ada, CEO</p>";
    expect(swapSignature(edited, a, b)).toBe(edited);
  });

  it("does not treat $ in a signature as a replacement pattern", () => {
    const cost = signatureBlock("Fee: $& $1");
    expect(swapSignature(`<p>Hi</p>${a}`, a, cost)).toBe(`<p>Hi</p>${cost}`);
  });
});
