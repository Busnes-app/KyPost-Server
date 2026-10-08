import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { HttpError } from "./client";
import { STEP_UP_HEADER, stepUpChallenge, withSSOStepUp } from "./stepup";

const postJSON = vi.fn();
const getJSON = vi.fn();
const deleteJSON = vi.fn();

vi.mock("./client", () => ({
  HttpError: class HttpError extends Error {
    readonly status: number;
    readonly body: unknown;
    constructor(message: string, status: number, body: unknown) {
      super(message);
      this.status = status;
      this.body = body;
    }
  },
  postJSON: (...args: unknown[]) => postJSON(...args),
  getJSON: (...args: unknown[]) => getJSON(...args),
  deleteJSON: (...args: unknown[]) => deleteJSON(...args)
}));

const refusal = () => new HttpError("request failed: 403", 403, { error: "sso_step_up_required", challenge: "rea_1" });

type FakePopup = { opener: unknown; closed: boolean; close: () => void; location: { href: string } };

function fakePopup(): FakePopup {
  return { opener: {}, closed: false, close: vi.fn(), location: { href: "" } };
}

beforeEach(() => {
  postJSON.mockReset();
  getJSON.mockReset();
  deleteJSON.mockReset();
  deleteJSON.mockResolvedValue(undefined);
  // jsdom has no <dialog> implementation.
  HTMLDialogElement.prototype.showModal = function () {
    this.setAttribute("open", "");
  };
  HTMLDialogElement.prototype.close = function () {
    this.removeAttribute("open");
  };
});

afterEach(() => {
  vi.unstubAllGlobals();
  document.body.innerHTML = "";
});

describe("stepUpChallenge", () => {
  it("reads the challenge off a step-up refusal and nothing else", () => {
    expect(stepUpChallenge(refusal())).toBe("rea_1");
    expect(stepUpChallenge(new HttpError("request failed: 403", 403, { error: "forbidden" }))).toBeNull();
    expect(stepUpChallenge(new HttpError("request failed: 401", 401, { error: "sso_step_up_required", challenge: "x" }))).toBeNull();
    expect(stepUpChallenge(new Error("offline"))).toBeNull();
  });
});

describe("withSSOStepUp", () => {
  it("passes an ordinary result and an ordinary error straight through", async () => {
    const run = vi.fn().mockResolvedValue("done");
    expect(await withSSOStepUp(run)).toBe("done");
    expect(run).toHaveBeenCalledWith({});

    const failing = vi.fn().mockRejectedValue(new Error("offline"));
    await expect(withSSOStepUp(failing)).rejects.toThrow("offline");
    expect(document.querySelector("dialog")).toBeNull();
  });

  it("proves the action in a detached popup and replays with the grant", async () => {
    const popup = fakePopup();
    let navigatedWithOpener: unknown = "unset";
    Object.defineProperty(popup.location, "href", {
      set(value: string) {
        navigatedWithOpener = popup.opener;
        expect(value).toBe("https://idp.example/authorize?prompt=login");
      }
    });
    vi.stubGlobal("open", vi.fn(() => popup));
    postJSON.mockResolvedValue({ authorizeUrl: "https://idp.example/authorize?prompt=login" });
    getJSON.mockResolvedValueOnce({ verified: false }).mockResolvedValueOnce({ verified: true });

    const run = vi.fn().mockRejectedValueOnce(refusal()).mockResolvedValueOnce("done");
    const pending = withSSOStepUp(run);
    const proceed = await vi.waitFor(() => {
      const button = [...document.querySelectorAll("dialog button")].find((b) => b.textContent === "Continue to KySignOn");
      if (!button) throw new Error("dialog not shown");
      return button as HTMLButtonElement;
    });
    proceed.click();

    expect(await pending).toBe("done");
    expect(navigatedWithOpener).toBeNull();
    expect(postJSON).toHaveBeenCalledWith("/api/auth/oidc/step-up", { challenge: "rea_1" });
    expect(run).toHaveBeenLastCalledWith({ [STEP_UP_HEADER]: "rea_1" });
    expect(popup.close).toHaveBeenCalled();
    expect(deleteJSON).not.toHaveBeenCalled();
    expect(document.querySelector("dialog")).toBeNull();
  });

  it("cancels the challenge on the server when the user gives up", async () => {
    const run = vi.fn().mockRejectedValueOnce(refusal());
    const pending = withSSOStepUp(run);
    const cancel = await vi.waitFor(() => {
      const button = [...document.querySelectorAll("dialog button")].find((b) => b.textContent === "Cancel");
      if (!button) throw new Error("dialog not shown");
      return button as HTMLButtonElement;
    });
    cancel.click();

    await expect(pending).rejects.toThrow("confirmation cancelled");
    expect(run).toHaveBeenCalledTimes(1);
    expect(postJSON).not.toHaveBeenCalled();
    expect(deleteJSON).toHaveBeenCalledWith("/api/auth/oidc/step-up/rea_1");
  });
});

// Under COOP the opener sees the popup as closed once it navigates cross-origin.
// Real timers: each poll sleeps one second.
describe("confirmation polling", () => {
  const button = (label: string) =>
    vi.waitFor(() => {
      const found = [...document.querySelectorAll<HTMLButtonElement>("dialog button")].find((b) => b.textContent === label);
      if (!found) throw new Error(`${label} not shown`);
      return found;
    });
  async function started() {
    const popup = fakePopup();
    Object.defineProperty(popup.location, "href", { set() { popup.closed = true; } });
    vi.stubGlobal("open", vi.fn(() => popup));
    postJSON.mockResolvedValue({ authorizeUrl: "https://idp.example/authorize" });
    const run = vi.fn().mockRejectedValueOnce(refusal()).mockResolvedValueOnce("done");
    const pending = withSSOStepUp(run);
    pending.catch(() => undefined);
    (await button("Continue to KySignOn")).click();
    await vi.waitFor(() => expect(getJSON).toHaveBeenCalled());
    // Wrapped: returning the promise itself would make `await started()` wait for it.
    return { pending };
  }
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("keeps polling after the popup reads as closed until the server says verified", async () => {
    getJSON.mockResolvedValueOnce({ verified: false }).mockResolvedValueOnce({ verified: false }).mockResolvedValueOnce({ verified: true });
    const { pending } = await started();
    expect(await pending).toBe("done");
    expect(getJSON).toHaveBeenCalledTimes(3);
    expect(deleteJSON).not.toHaveBeenCalled();
  });

  it("drops the challenge when the user cancels while waiting", async () => {
    getJSON.mockResolvedValue({ verified: false });
    const { pending } = await started();
    (await button("Cancel")).click();
    await expect(pending).rejects.toThrow("confirmation cancelled");
    expect(deleteJSON).toHaveBeenCalledWith("/api/auth/oidc/step-up/rea_1");
  });

  it("drops the challenge at the deadline", async () => {
    const realNow = Date.now.bind(Date);
    getJSON.mockImplementation(async () => {
      // The first poll ends past the five-minute window.
      vi.spyOn(Date, "now").mockImplementation(() => realNow() + 5 * 60_000 + 1);
      return { verified: false };
    });
    const { pending } = await started();
    await expect(pending).rejects.toThrow("confirmation cancelled or expired");
    expect(getJSON).toHaveBeenCalledTimes(1);
    expect(deleteJSON).toHaveBeenCalledWith("/api/auth/oidc/step-up/rea_1");
  });
});
