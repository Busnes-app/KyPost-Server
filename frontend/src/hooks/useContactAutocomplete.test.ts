import { afterEach, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";

const contacts = vi.hoisted(() => ({ list: [] as Array<{ uid: string; fn: string; emails?: Array<{ value: string }> }> }));
vi.mock("../api/contacts", () => ({ searchContacts: async () => ({ contacts: contacts.list }) }));
const people = vi.hoisted(() => ({ answer: (): Promise<unknown> => Promise.resolve({ people: [] }) }));
const getJSON = vi.hoisted(() => vi.fn((_path: string) => people.answer()));
vi.mock("../api/client", () => ({ getJSON }));

import { useContactAutocomplete } from "./useContactAutocomplete";

afterEach(() => vi.clearAllMocks());

it("lists contacts first, then organisation people, deduplicated by address", async () => {
  contacts.list = [{ uid: "c1", fn: "Alice (mine)", emails: [{ value: "Alice@Example.com" }] }, { uid: "c2", fn: "No address" }];
  people.answer = async () => ({ people: [
    { sub: "s1", name: "Alice Example", email: "alice@example.com" },
    { sub: "s2", name: "Albert Org", email: "albert@example.com" },
    { sub: "s3", name: "Bad", email: 7 }
  ] });
  const { result } = renderHook(() => useContactAutocomplete("al", true));
  await waitFor(() => expect(result.current.searched).toBe(true));
  expect(result.current.results).toEqual([
    { key: "c1", name: "Alice (mine)", email: "Alice@Example.com", organisation: false },
    { key: "org:albert@example.com", name: "Albert Org", email: "albert@example.com", organisation: true }
  ]);
  expect(getJSON).toHaveBeenCalledWith("/api/suite/people?q=al");
});

it("keeps contacts when the organisation search fails, and skips it for one character", async () => {
  contacts.list = [{ uid: "c1", fn: "Ann", emails: [{ value: "ann@example.com" }] }];
  people.answer = async () => { throw new Error("down"); };
  const { result } = renderHook(() => useContactAutocomplete("an", true));
  await waitFor(() => expect(result.current.searched).toBe(true));
  expect(result.current.results.map((r) => r.email)).toEqual(["ann@example.com"]);

  getJSON.mockClear();
  const one = renderHook(() => useContactAutocomplete("a", true));
  await waitFor(() => expect(one.result.current.searched).toBe(true));
  expect(getJSON).not.toHaveBeenCalled();
});
