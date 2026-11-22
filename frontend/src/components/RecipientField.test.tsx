import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { RecipientField } from "./RecipientField";
import { isDuplicateInField, type RecipientFieldState, type RecipientToken } from "../lib/recipients";
import type { PGPRecipientTier } from "../api/pgp";

// Every behaviour the hand-rolled field had, plus the combobox semantics it
// lacked, against the same endpoints useContactAutocomplete calls.

const json = (body: unknown) => new Response(JSON.stringify(body), { status: 200, headers: { "Content-Type": "application/json" } });
let statusBodies: unknown[] = [];

beforeEach(() => {
  statusBodies = [];
  vi.spyOn(globalThis, "fetch").mockImplementation(async (input, init) => {
    const url = String(input);
    if (url.startsWith("/api/contacts/search")) {
      return json({ contacts: [
        { uid: "c1", fn: "Alice (mine)", emails: [{ value: "alice@example.com" }] },
        { uid: "c2", fn: "Alan Ames", emails: [{ value: "alan@example.com" }] }
      ] });
    }
    if (url.startsWith("/api/suite/people")) {
      return json({ people: [
        { name: "<b>Albert</b>", email: "albert@example.com" },
        { name: "", email: "alma@example.com" },
        { name: "Alice again", email: "ALICE@example.com" }
      ] });
    }
    if (url === "/api/kymessage/status") {
      statusBodies.push(JSON.parse(String(init?.body)));
      return json({ people: [
        { address: "albert@example.com", state: "in_a_meeting", until: "2026-11-16T15:00:00Z" },
        { address: "alma@example.com", state: "away" }
      ] });
    }
    return new Response("not found", { status: 404 });
  });
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
});

/** The field as compose wires it: useCompose refuses a duplicate with a notice. */
function Harness({ initial = [], tiers, onAdd }: { initial?: RecipientToken[]; tiers?: Record<string, PGPRecipientTier>; onAdd?: (t: RecipientToken) => void }) {
  const [state, setState] = useState<RecipientFieldState>({ tokens: initial, draft: "" });
  const [notice, setNotice] = useState("");
  return (
    <>
      <RecipientField
        label="To"
        state={state}
        tiers={tiers}
        onDraftChange={(draft) => setState((prev) => ({ ...prev, draft }))}
        onAddToken={(token) => {
          onAdd?.(token);
          setState((prev) => {
            if (isDuplicateInField(prev.tokens, token.email)) {
              setNotice(`${token.name ?? token.email} is already in TO.`);
              return prev;
            }
            return { tokens: [...prev.tokens, token], draft: "" };
          });
        }}
        onRemoveToken={(index) => setState((prev) => ({ ...prev, tokens: prev.tokens.filter((_, i) => i !== index) }))}
      />
      <p data-testid="notice">{notice}</p>
      <p data-testid="draft">{state.draft}</p>
      <button type="button">after</button>
    </>
  );
}

const chips = () => [...document.querySelectorAll(".compose-token-pill-label")].map((c) => c.textContent);
// While the list is open Combobox hides the rest of the page from assistive
// tech (aria-hidden), so "after" is found by its text.
const box = () => screen.getByRole("combobox", { name: "To recipients" });
const option = (name: RegExp) => screen.findByRole("option", { name });

async function typeAndWait(user: ReturnType<typeof userEvent.setup>, text: string) {
  await user.type(box(), text);
  await screen.findByRole("option", { name: /^Alice \(mine\)/ });
}

it("is a combobox whose suggestions are options, named by aria-activedescendant", async () => {
  const user = userEvent.setup();
  render(<Harness />);
  expect(box().getAttribute("aria-expanded")).toBe("false");
  await typeAndWait(user, "al");
  expect(box().getAttribute("aria-expanded")).toBe("true");
  expect(box().getAttribute("aria-controls")).toBe(screen.getByRole("listbox").id);
  // The first suggestion is highlighted as the list arrives, as activeIndex 0 was.
  await waitFor(() => expect(box().getAttribute("aria-activedescendant")).toBe((screen.getAllByRole("option")[0]).id));
  expect(document.activeElement).toBe(box());
});

it("puts organisation people under one Organisation heading, as text, deduplicated by address", async () => {
  const user = userEvent.setup();
  render(<Harness />);
  await typeAndWait(user, "al");
  await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(4));
  expect(screen.getAllByText("Organisation")).toHaveLength(1);
  const group = screen.getByRole("group", { name: "Organisation" });
  expect(within(group).getAllByRole("option").map((o) => o.textContent?.replace(/(In a meeting until|Away).*/, ""))).toEqual(["<b>Albert</b>albert@example.com", "alma@example.com"]);
  expect(document.querySelector(".recipient-autocomplete b")).toBeNull();
  expect(screen.getAllByRole("option").map((o) => o.textContent?.slice(0, 12))).toEqual(["Alice (mine)", "Alan Amesala", "<b>Albert</b", "alma@example"]);
  // The typed text is marked in the name and the address.
  expect([...document.querySelectorAll(".recipient-autocomplete-match")].map((m) => m.textContent)).toContain("Al");
});

it("shows organisation people's status in words, and asks only for them", async () => {
  const user = userEvent.setup();
  render(<Harness />);
  await typeAndWait(user, "al");
  expect(await screen.findByText(/^In a meeting until /)).toBeTruthy();
  const away = screen.getByText("Away");
  expect(away.getAttribute("data-state")).toBe("away");
  expect(away.querySelector(".recipient-status-dot")?.getAttribute("aria-hidden")).toBe("true");
  expect(statusBodies).toEqual([{ addresses: ["albert@example.com", "alma@example.com"] }]);
});

it("Enter accepts the highlighted suggestion; arrows move it and stop at the ends", async () => {
  const user = userEvent.setup();
  render(<Harness />);
  await typeAndWait(user, "al");
  await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(4));
  const ids = screen.getAllByRole("option").map((o) => o.id);
  const active = () => box().getAttribute("aria-activedescendant");
  await waitFor(() => expect(active()).toBe(ids[0]));
  await user.keyboard("{ArrowUp}");
  expect(active()).toBe(ids[0]);
  await user.keyboard("{ArrowDown}{ArrowDown}{ArrowDown}{ArrowDown}{ArrowDown}");
  expect(active()).toBe(ids[3]);
  await user.keyboard("{ArrowUp}{ArrowUp}{Enter}");
  expect(chips()).toEqual(["Alan Ames <alan@example.com>"]);
  expect(box()).toHaveProperty("value", "");
  await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
  expect(document.activeElement).toBe(box());
});

it("Tab accepts the highlighted suggestion and moves on", async () => {
  const user = userEvent.setup();
  render(<Harness />);
  await typeAndWait(user, "al");
  await waitFor(() => expect(box().getAttribute("aria-activedescendant")).toBeTruthy());
  await user.keyboard("{ArrowDown}");
  await user.tab();
  expect(chips()).toEqual(["Alan Ames <alan@example.com>"]);
  expect(document.activeElement).toBe(screen.getByText("after"));
});

it("Tab never accepts a suggestion that is no longer listed (a stale highlight)", async () => {
  const user = userEvent.setup();
  render(<Harness />);
  await typeAndWait(user, "al");
  await waitFor(() => expect(box().getAttribute("aria-activedescendant")).toBeTruthy());
  await user.keyboard("{ArrowDown}{Enter}");
  expect(chips()).toEqual(["Alan Ames <alan@example.com>"]);
  // The next search is still pending (debounced): no options are listed yet,
  // so Tab must leave the field and commit the typed address on blur.
  await user.type(box(), "zz@q.test");
  expect(screen.queryAllByRole("option")).toHaveLength(0);
  await user.tab();
  expect(chips()).toEqual(["Alan Ames <alan@example.com>", "zz@q.test"]);
  expect(screen.getByTestId("notice").textContent).toBe("");
});

it("a click on a suggestion chooses it; an organisation person without a name is their address", async () => {
  const user = userEvent.setup();
  const onAdd = vi.fn();
  render(<Harness onAdd={onAdd} />);
  await typeAndWait(user, "al");
  await user.click(await option(/^alma@example\.com/));
  expect(onAdd).toHaveBeenCalledWith({ email: "alma@example.com", name: undefined, isCustom: false });
  expect(chips()).toEqual(["alma@example.com"]);
});

it("choosing someone already in the field leaves it alone and says so", async () => {
  const user = userEvent.setup();
  render(<Harness initial={[{ email: "alice@example.com", isCustom: true }]} />);
  await typeAndWait(user, "al");
  await waitFor(() => expect(box().getAttribute("aria-activedescendant")).toBeTruthy());
  await user.keyboard("{Enter}");
  expect(chips()).toEqual(["alice@example.com"]);
  expect(screen.getByTestId("notice").textContent).toBe("Alice (mine) is already in TO.");
});

it("Escape closes the list and keeps the draft and chips; with the list closed it is left to the dialog", async () => {
  const user = userEvent.setup();
  render(<Harness initial={[{ email: "x@y.test", isCustom: true }]} />);
  await typeAndWait(user, "al");
  const open = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
  fireEvent(box(), open);
  // preventDefault stops a native <dialog> from cancelling on the same key.
  expect(open.defaultPrevented).toBe(true);
  await waitFor(() => expect(screen.queryByRole("listbox")).toBeNull());
  expect(box()).toHaveProperty("value", "al");
  expect(chips()).toEqual(["x@y.test"]);

  const closed = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
  fireEvent(box(), closed);
  expect(closed.defaultPrevented).toBe(false);
  expect(box()).toHaveProperty("value", "al");
  expect(chips()).toEqual(["x@y.test"]);

  // Typing again reopens it.
  await user.type(box(), "i");
  expect(await option(/^Alice \(mine\)/)).toBeTruthy();
});

it("comma or semicolon turns a typed address into a chip; a bad one is refused with a message", async () => {
  const user = userEvent.setup();
  render(<Harness />);
  await user.type(box(), "x@y.test,");
  await user.type(box(), "z@y.test;");
  expect(chips()).toEqual(["x@y.test", "z@y.test"]);
  expect(box()).toHaveProperty("value", "");

  await user.type(box(), "nope,");
  expect(screen.getByText('"nope" doesn\'t look like a valid email address.')).toBeTruthy();
  expect(box()).toHaveProperty("value", "nope");
  expect(chips()).toEqual(["x@y.test", "z@y.test"]);
  await user.type(box(), "@x.test");
  expect(screen.queryByText(/doesn't look like a valid email/)).toBeNull();
});

it("leaving the field commits a typed address, or explains why not", async () => {
  const user = userEvent.setup();
  render(<Harness />);
  await user.type(box(), "q@r.test");
  await user.click(screen.getByText("after"));
  expect(chips()).toEqual(["q@r.test"]);

  await user.type(box(), "bad");
  await user.click(screen.getByText("after"));
  expect(screen.getByText('"bad" doesn\'t look like a valid email address.')).toBeTruthy();
  expect(chips()).toEqual(["q@r.test"]);
});

it("a pasted list stays in the box as pasted; send splits it (serializeRecipientField)", async () => {
  const user = userEvent.setup();
  render(<Harness />);
  await user.click(box());
  await user.paste("a@b.test, c@d.test; e@f.test");
  expect(screen.getByTestId("draft").textContent).toBe("a@b.test, c@d.test; e@f.test");
  expect(chips()).toEqual([]);
});

it("Backspace in an empty box removes the last chip; Backspace or Delete on a chip removes that one", async () => {
  const user = userEvent.setup();
  render(<Harness initial={[{ email: "a@b.test", isCustom: true }, { email: "c@d.test", isCustom: true }, { email: "e@f.test", isCustom: true }]} />);
  await user.click(box());
  await user.keyboard("{Backspace}");
  expect(chips()).toEqual(["a@b.test", "c@d.test"]);
  // Left from the start of the box walks onto the chips.
  await user.keyboard("{ArrowLeft}{ArrowLeft}");
  expect(document.activeElement?.textContent).toContain("a@b.test");
  await user.keyboard("{Delete}");
  expect(chips()).toEqual(["c@d.test"]);
  // Backspace with text in the box edits the text, not the chips.
  await user.click(box());
  await user.keyboard("zz{Backspace}");
  expect(box()).toHaveProperty("value", "z");
  expect(chips()).toEqual(["c@d.test"]);
});

it("each chip's x removes it, and stays a Tab stop", async () => {
  const user = userEvent.setup();
  render(<Harness initial={[{ email: "a@b.test", name: "Ann", isCustom: false }, { email: "c@d.test", isCustom: true }]} />);
  expect(chips()).toEqual(["Ann <a@b.test>", "c@d.test"]);
  const remove = screen.getByRole("button", { name: "Remove a@b.test" });
  expect(remove.tabIndex).toBe(0);
  await user.click(remove);
  expect(chips()).toEqual(["c@d.test"]);
  // Keyboard: Shift+Tab from the box reaches the remaining x; Enter removes.
  await user.click(box());
  await user.tab({ shift: true });
  expect(document.activeElement).toBe(screen.getByRole("button", { name: "Remove c@d.test" }));
  await user.keyboard("{Enter}");
  expect(chips()).toEqual([]);
});

it("shows the placeholder only while empty, and each chip's key tier", () => {
  const { unmount } = render(<Harness />);
  expect(box().getAttribute("placeholder")).toBe("To recipients");
  unmount();
  render(<Harness initial={[{ email: "A@b.test", isCustom: true }, { email: "c@d.test", isCustom: true }]} tiers={{ "a@b.test": "verified", "c@d.test": "none" }} />);
  expect(box().getAttribute("placeholder")).toBe("");
  expect([...document.querySelectorAll(".compose-token-pill-badge")].map((b) => b.textContent)).toEqual(["🔒 verified", "no key — pickup link"]);
});

it("No matches when nothing is found", async () => {
  vi.mocked(globalThis.fetch).mockImplementation(async (input) => String(input).startsWith("/api/contacts/search") ? json({ contacts: [] }) : json({ people: [] }));
  const user = userEvent.setup();
  render(<Harness />);
  await user.type(box(), "zz");
  expect(await screen.findByText("No matches")).toBeTruthy();
  expect(screen.queryAllByRole("option")).toHaveLength(0);
});
