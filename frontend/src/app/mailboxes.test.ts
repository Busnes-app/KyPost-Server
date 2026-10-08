import { afterEach, describe, expect, it } from "vitest";
import { loadMailboxSelection, readMailboxes, saveMailboxSelection } from "./mailboxes";

const TEAM = "mbx-11111111-2222-4333-8444-555555555555";
const primary = { id: "u1", kind: "primary", addresses: [{ address: "me@urlxl.us", kind: "primary" }] };
const team = { id: TEAM, kind: "extra", addresses: [{ address: "a@urlxl.us", kind: "alias" }, { address: "team@urlxl.us", kind: "primary" }] };

describe("readMailboxes", () => {
  it("reads the primary then extras, primary address first", () => {
    expect(readMailboxes({ mailboxes: [primary, team] }, "u1")).toEqual([
      { id: "u1", kind: "primary", addresses: ["me@urlxl.us"] },
      { id: TEAM, kind: "extra", addresses: ["team@urlxl.us", "a@urlxl.us"] }
    ]);
    expect(readMailboxes({ mailboxes: [] }, "u1")).toEqual([]);
  });

  it.each([
    ["no list", {}],
    ["primary not first", { mailboxes: [team, primary] }],
    ["another user's primary", { mailboxes: [{ ...primary, id: "u2" }] }],
    ["an extra ID that is not a mailbox ID", { mailboxes: [primary, { ...team, id: "../u2" }] }],
    ["a repeated extra", { mailboxes: [primary, team, team] }],
    ["an address in two mailboxes", { mailboxes: [primary, { ...team, addresses: [{ address: "me@urlxl.us", kind: "primary" }] }] }],
    ["two primary addresses", { mailboxes: [primary, { ...team, addresses: [{ address: "x@urlxl.us", kind: "primary" }, { address: "y@urlxl.us", kind: "primary" }] }] }],
    ["an unknown address kind", { mailboxes: [primary, { ...team, addresses: [{ address: "x@urlxl.us", kind: "owner" }] }] }],
    ["a display-name address", { mailboxes: [{ ...primary, addresses: [{ address: "Me <me@urlxl.us>", kind: "primary" }] }] }]
  ])("refuses %s", (_name, body) => {
    expect(() => readMailboxes(body, "u1")).toThrow("Invalid mailbox list.");
  });
});

describe("mailbox selection storage", () => {
  afterEach(() => window.sessionStorage.clear());

  it("is per user, per tab, and only ever an extra mailbox ID", () => {
    saveMailboxSelection("u1", TEAM);
    expect(loadMailboxSelection("u1")).toBe(TEAM);
    expect(loadMailboxSelection("u2")).toBe("");
    expect(window.localStorage.length).toBe(0);
    saveMailboxSelection("u1", "");
    expect(loadMailboxSelection("u1")).toBe("");
    window.sessionStorage.setItem("kypost-mailbox:u1", "u1");
    expect(loadMailboxSelection("u1")).toBe("");
  });
});
