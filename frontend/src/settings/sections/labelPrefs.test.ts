import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  descriptionsToText,
  loadLabelPrefs,
  saveLabelPrefsPatch,
  SORTER_MAX_LABELS,
  sorterLabelLimitNote,
  textToDescriptions
} from "./labelPrefs";

const getJSON = vi.fn();
const putJSON = vi.fn();

vi.mock("../../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  putJSON: (url: string, body: unknown) => putJSON(url, body)
}));

beforeEach(() => {
  getJSON.mockReset();
  putJSON.mockReset();
  putJSON.mockResolvedValue({ ok: true });
});

describe("saveLabelPrefsPatch", () => {
  it("sends the whole label block, not just the patch", async () => {
    // The endpoint replaces the account's entire label document. The auto-apply
    // toggle and the label list are separate controls writing to it, so a
    // caller sending only its own field would blank the other's.
    getJSON.mockResolvedValue({
      autoApplyEnabled: true,
      seeded: true,
      allowlist: ["Primary", "Promotions"],
      keywordMappings: { Primary: ["Primary", "Important"] }
    });

    await saveLabelPrefsPatch({ autoApplyEnabled: false });

    const [url, body] = putJSON.mock.calls[0];
    expect(url).toBe("/api/labels/preferences");
    expect(body).toMatchObject({
      autoApplyEnabled: false,
      allowlist: ["Primary", "Promotions"],
      keywordMappings: { Primary: ["Primary", "Important"] }
    });
  });

  it("reads fresh before writing, so one tab cannot save the other's stale copy", async () => {
    getJSON.mockResolvedValue({ autoApplyEnabled: true, seeded: true, allowlist: ["Later"], keywordMappings: {} });

    await saveLabelPrefsPatch({ autoApplyEnabled: false });

    expect(getJSON).toHaveBeenCalledWith("/api/labels/preferences");
    const [, body] = putJSON.mock.calls[0];
    expect(body.allowlist).toEqual(["Later"]);
  });

  it("saves an empty allowlist when that is what was asked for", async () => {
    // Deliberately clearing every label must reach the server as an empty
    // list, not be mistaken for "no change".
    getJSON.mockResolvedValue({ autoApplyEnabled: true, seeded: true, allowlist: ["Primary"], keywordMappings: {} });

    await saveLabelPrefsPatch({ allowlist: [], keywordMappings: {} });

    const [, body] = putJSON.mock.calls[0];
    expect(body.allowlist).toEqual([]);
  });
});

describe("loadLabelPrefs", () => {
  it("fills in fields an older server omits", async () => {
    getJSON.mockResolvedValue({ autoApplyEnabled: false });

    const prefs = await loadLabelPrefs();

    expect(prefs.allowlist).toEqual([]);
    expect(prefs.keywordMappings).toEqual({});
    expect(prefs.descriptions).toEqual({});
    expect(prefs.autoApplyEnabled).toBe(false);
  });
});

describe("label descriptions text", () => {
  it("round-trips, splitting only on the first colon", () => {
    const descriptions = { Receipts: "orders: confirmations, invoices", Primary: "people writing to me" };
    expect(textToDescriptions(descriptionsToText(descriptions), ["Primary", "Receipts"])).toEqual(descriptions);
  });

  it("drops blank lines, empty descriptions and labels not in the list", () => {
    // The server refuses a description for a label that is not in the list, so
    // a label removed from the allowlist must not take the whole save down.
    const text = "\nReceipts:   \nGone: an old label\nNo colon here\n  Updates : account notices  ";
    expect(textToDescriptions(text, ["Receipts", "Updates"])).toEqual({ Updates: "account notices" });
  });
});

describe("sorterLabelLimitNote", () => {
  it("is silent up to the limit and warns past it", () => {
    // Past the limit the account silently loses the on-device sorter; the
    // label form is where the user must find that out.
    expect(sorterLabelLimitNote(0)).toBeNull();
    expect(sorterLabelLimitNote(SORTER_MAX_LABELS)).toBeNull();
    const note = sorterLabelLimitNote(SORTER_MAX_LABELS + 1);
    expect(note).toContain(`${SORTER_MAX_LABELS + 1} labels`);
    expect(note).toContain(`up to ${SORTER_MAX_LABELS}`);
  });
});
