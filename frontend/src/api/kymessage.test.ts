import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { chatOrigin, forgetChatPeople, isChatPerson, openChat, statusText } from "./kymessage";

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
const fetchMock = vi.fn<typeof fetch>();
const bodyOf = (i: number) => JSON.parse(String(fetchMock.mock.calls[i][1]?.body));

beforeEach(() => {
  forgetChatPeople();
  vi.stubGlobal("fetch", fetchMock);
});
afterEach(() => {
  vi.resetAllMocks();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe("chatOrigin", () => {
  it("is the https origin of Element's suite entry only", () => {
    const app = (id: string, url: string) => ({ id, name: id, url });
    expect(chatOrigin([app("kypost", "https://mail.example/"), app("kymessage-chat", "https://chat.example/home")])).toBe("https://chat.example");
    expect(chatOrigin([app("kymessage", "https://console.example/")])).toBe("");
    expect(chatOrigin([app("kymessage-chat", "http://chat.example/")])).toBe("");
    expect(chatOrigin([app("kymessage-chat", "https://u:p@chat.example/")])).toBe("");
    expect(chatOrigin([app("kymessage-chat", "not a url")])).toBe("");
    expect(chatOrigin([])).toBe("");
  });
});

describe("isChatPerson", () => {
  it("asks once for every address in a tick, then remembers", async () => {
    fetchMock.mockResolvedValue(json({ inside: ["bob@example.com"] }));
    const answers = await Promise.all([isChatPerson("Bob@Example.com"), isChatPerson("out@other.org"), isChatPerson("bob@example.com")]);
    expect(answers).toEqual([true, false, true]);
    expect(fetchMock).toHaveBeenCalledTimes(1);
    expect(fetchMock.mock.calls[0][0]).toBe("/api/kymessage/people");
    expect(bodyOf(0)).toEqual({ addresses: ["bob@example.com", "out@other.org"] });
    expect(await isChatPerson("out@other.org")).toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it("reads a failure as no Chat, and asks again next time", async () => {
    fetchMock.mockResolvedValueOnce(json({ code: "kymessage_off" }, 409));
    expect(await isChatPerson("bob@example.com")).toBe(false);
    fetchMock.mockResolvedValueOnce(json({ inside: ["bob@example.com"] }));
    expect(await isChatPerson("bob@example.com")).toBe(true);
    expect(await isChatPerson("no-at-sign")).toBe(false);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });
});

describe("openChat", () => {
  const fakeTab = () => ({ opener: {} as unknown, location: { replace: vi.fn() }, close: vi.fn() });

  it("opens a tab inside the click, cuts it off from this window, and points it at the answer", async () => {
    const tab = fakeTab();
    const open = vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
    fetchMock.mockResolvedValue(json({ url: "https://chat.example/#/user/@bob:example.com" }));
    expect(await openChat("bob@example.com", "https://chat.example")).toBe("");
    expect(open).toHaveBeenCalledWith("", "_blank");
    expect(tab.opener).toBeNull();
    expect(tab.location.replace).toHaveBeenCalledWith("https://chat.example/#/user/@bob:example.com");
    expect(bodyOf(0)).toEqual({ address: "bob@example.com" });
  });

  it("closes the tab and says why on a refusal or a link off Element's origin", async () => {
    for (const [answer, message] of [
      [json({ error: "x", code: "no_target" }, 404), "This person isn't on KyMessages yet."],
      [json({ error: "x", code: "kymessage_unavailable" }, 502), "Could not open KyMessages. Try again."],
      [json({ url: "https://evil.example/#/user/@bob:example.com" }), "Could not open KyMessages. Try again."],
      [json({ url: "javascript:alert(1)" }), "Could not open KyMessages. Try again."]
    ] as const) {
      const tab = fakeTab();
      vi.spyOn(window, "open").mockReturnValue(tab as unknown as Window);
      fetchMock.mockResolvedValueOnce(answer);
      expect(await openChat("bob@example.com", "https://chat.example")).toBe(message);
      expect(tab.close).toHaveBeenCalled();
      expect(tab.location.replace).not.toHaveBeenCalled();
    }
  });
});

describe("statusText", () => {
  it("names every state and gives meetings and out-of-office their end", () => {
    const end = new Date(2026, 10, 16, 15, 30);
    expect(statusText({ state: "in_a_meeting", until: end.toISOString() })).toBe(`In a meeting until ${end.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`);
    expect(statusText({ state: "out_of_office", until: end.toISOString() }, new Date(2026, 10, 1))).toMatch(/^Out of office until .*16/);
    expect(statusText({ state: "busy" })).toBe("Busy");
    expect(statusText({ state: "unknown" })).toBe("Status unknown");
  });
});
