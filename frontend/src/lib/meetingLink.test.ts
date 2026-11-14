import { describe, expect, it, vi } from "vitest";
import { calendarOrigin, newMeetingUrl, openNewMeeting } from "./meetingLink";

const app = (id: string, url: string) => ({ id, name: id, url });

describe("calendarOrigin", () => {
  it("is the https origin of KyCalendar's suite url only", () => {
    expect(calendarOrigin([app("kydrive", "https://drive.example"), app("kycalendar", "https://cal.example:8443/app/?x=1#y")])).toBe("https://cal.example:8443");
    expect(calendarOrigin([app("kydrive", "https://drive.example")])).toBe("");
    expect(calendarOrigin([app("kycalendar", "http://cal.example")])).toBe("");
    expect(calendarOrigin([app("kycalendar", "javascript:alert(1)")])).toBe("");
    expect(calendarOrigin([app("kycalendar", "not a url")])).toBe("");
  });
});

describe("newMeetingUrl", () => {
  const parse = (url: string) => new URL(url).searchParams;

  it("encodes every value and keeps the path fixed", () => {
    const url = newMeetingUrl("https://cal.example", { title: "Q4 & review?#x", attendees: ["bob@example.net", "Carol@Example.org"], body: "a=1&b=2\nline two" });
    expect(url.startsWith("https://cal.example/new?")).toBe(true);
    const q = parse(url);
    expect(q.get("title")).toBe("Q4 & review?#x");
    expect(q.get("attendees")).toBe("bob@example.net,Carol@Example.org");
    expect(q.get("body")).toBe("a=1&b=2\nline two");
    expect(new URL(url).hash).toBe("");
  });

  it("drops invalid and duplicate attendees and caps them at 100", () => {
    const q = parse(newMeetingUrl("https://cal.example", { attendees: ["Bob <bob@x.test>", "a,b@x.test", "bob@x.test", "BOB@x.test", " c@x.test ", "no-at", "d@x.test‮"] }));
    expect(q.get("attendees")).toBe("bob@x.test,c@x.test");
    const many = Array.from({ length: 150 }, (_, i) => `p${i}@x.test`);
    expect(parse(newMeetingUrl("https://cal.example", { attendees: many })).get("attendees")!.split(",")).toHaveLength(100);
  });

  it("caps title at 255 and body at 2000 UTF-8 bytes without splitting a character", () => {
    const q = parse(newMeetingUrl("https://cal.example", { title: "é".repeat(200), body: "€".repeat(1000) }));
    expect(new TextEncoder().encode(q.get("title")!).length).toBe(254);
    expect(q.get("title")).toBe("é".repeat(127));
    expect(new TextEncoder().encode(q.get("body")!).length).toBe(1998);
  });

  it("strips controls from the title and omits empty values", () => {
    expect(parse(newMeetingUrl("https://cal.example", { title: "a\r\nb\u0000c‮" })).get("title")).toBe("a b c");
    expect(newMeetingUrl("https://cal.example", { title: "  ", attendees: [], body: "" })).toBe("https://cal.example/new");
  });
});

describe("openNewMeeting", () => {
  it("opens a new tab with no opener or referrer", () => {
    const open = vi.spyOn(window, "open").mockReturnValue(null);
    openNewMeeting("https://cal.example", { title: "x" });
    expect(open).toHaveBeenCalledWith("https://cal.example/new?title=x", "_blank", "noopener,noreferrer");
    open.mockRestore();
  });
});
