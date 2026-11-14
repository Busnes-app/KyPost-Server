import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";

const client = vi.hoisted(() => ({ getBytes: vi.fn(), postJSON: vi.fn() }));
vi.mock("../../api/client", async (importOriginal) => ({ ...(await importOriginal<object>()), ...client }));

import { HttpError } from "../../api/client";
import { InviteCard } from "./InviteCard";

const invite = (method = "REQUEST", extra = "") => new TextEncoder().encode(
  `BEGIN:VCALENDAR\r\nMETHOD:${method}\r\nBEGIN:VEVENT\r\nUID:u1\r\nSEQUENCE:1\r\nSUMMARY:<img src=x onerror=alert(1)>\r\n` +
  `LOCATION:Room 1\r\nDTSTART:20261020T100000Z\r\nDTEND:20261020T110000Z\r\nORGANIZER:mailto:alice@example.com\r\n` +
  `ATTENDEE:mailto:bob@example.net\r\n${extra}END:VEVENT\r\nEND:VCALENDAR\r\n`
);
const props = {
  attachment: { index: 1, name: "invite.ics", mimeType: "text/calendar", size: 300, calendarMethod: "REQUEST" },
  query: "messageId=42",
  messageId: "42",
  folder: "",
  sender: "alice@example.com",
  phishing: false,
  ownAddresses: ["bob@example.net"]
};
const refusal = (status: number, code: string) => new HttpError(`request failed: ${status}`, status, { code, error: `says ${code}` });

beforeEach(() => {
  window.localStorage.clear();
  client.getBytes.mockResolvedValue(invite());
  client.postJSON.mockReset();
});
afterEach(cleanup);

describe("InviteCard", () => {
  it("renders sender text as text, with the update warning", async () => {
    const { container } = render(<InviteCard {...props} />);
    expect(await screen.findByText("<img src=x onerror=alert(1)>")).toBeDefined();
    expect(container.querySelector("img")).toBeNull();
    expect(screen.getByText("Room 1")).toBeDefined();
    expect(screen.getByText("This updates an earlier invitation.")).toBeDefined();
    expect(client.getBytes).toHaveBeenCalledWith("/api/mail/attachment?messageId=42&index=1");
  });

  it("warns when the organizer is not the sender, and offers no answer to a cancellation", async () => {
    client.getBytes.mockResolvedValue(invite("CANCEL"));
    render(<InviteCard {...props} sender="mallory@example.org" phishing />);
    expect(await screen.findByText("The organizer is not the sender (mallory@example.org).")).toBeDefined();
    expect(screen.getByText("Cancelled")).toBeDefined();
    expect(screen.getByText("This message was flagged as possible phishing.")).toBeDefined();
    expect(screen.queryByRole("button", { name: "Accept" })).toBeNull();
  });

  it("offers no answer when the server did not list the part's METHOD", async () => {
    render(<InviteCard {...props} attachment={{ ...props.attachment, calendarMethod: undefined }} />);
    await screen.findByText("Room 1");
    expect(screen.queryByRole("button", { name: "Accept" })).toBeNull();
  });

  it("answers through KyCalendar and sends no mail", async () => {
    client.postJSON.mockResolvedValue({ outcome: "answered" });
    render(<InviteCard {...props} />);
    fireEvent.click(await screen.findByRole("button", { name: "Maybe" }));
    expect(await screen.findByText("Answered through KyCalendar.")).toBeDefined();
    expect(client.postJSON).toHaveBeenCalledTimes(1);
    expect(client.postJSON).toHaveBeenCalledWith("/api/calendar/rsvp", { messageId: "42", mailbox: "", index: 1, partstat: "TENTATIVE" });
    expect(screen.queryByRole("button", { name: "Accept" })).toBeNull();
  });

  it("falls back to a confirmed mail REPLY when KyCalendar is off", async () => {
    client.postJSON.mockRejectedValueOnce(refusal(409, "kycalendar_off")).mockResolvedValueOnce({ ok: true, calendarReply: true });
    render(<InviteCard {...props} />);
    fireEvent.click(await screen.findByRole("button", { name: "Decline" }));
    fireEvent.click(await screen.findByRole("button", { name: "Send answer" }));
    expect(await screen.findByText("Your answer was sent to alice@example.com.")).toBeDefined();
    const [path, body] = client.postJSON.mock.calls[1];
    expect(path).toBe("/api/mail/send");
    expect(body).toMatchObject({ to: "alice@example.com", from: "bob@example.net", encrypt: false, sign: false });
    expect(body.calendarReply.ics).toContain("ATTENDEE;PARTSTAT=DECLINED:mailto:bob@example.net\r\n");
  });

  it("offers the mail REPLY when KyCalendar has no event for the invitation", async () => {
    client.postJSON.mockRejectedValueOnce(refusal(404, "no_event")).mockResolvedValueOnce({ ok: true, calendarReply: true });
    render(<InviteCard {...props} />);
    fireEvent.click(await screen.findByRole("button", { name: "Accept" }));
    expect(await screen.findByText(/KyCalendar has no event for this invitation, so your answer goes to alice@example.com/)).toBeDefined();
    fireEvent.click(screen.getByRole("button", { name: "Send answer" }));
    expect(await screen.findByText("Your answer was sent to alice@example.com.")).toBeDefined();
    expect(client.postJSON.mock.calls[1][0]).toBe("/api/mail/send");
  });

  it("re-enables the buttons when KyCalendar is unavailable", async () => {
    client.postJSON.mockRejectedValueOnce(refusal(502, "kycalendar_unavailable"));
    render(<InviteCard {...props} />);
    fireEvent.click(await screen.findByRole("button", { name: "Accept" }));
    expect(await screen.findByText("says kycalendar_unavailable")).toBeDefined();
    expect((screen.getByRole("button", { name: "Accept" }) as HTMLButtonElement).disabled).toBe(false);
    expect(client.postJSON).toHaveBeenCalledTimes(1);
  });

  it("asks for a reload on conflict and reads the invitation again", async () => {
    client.postJSON.mockRejectedValueOnce(refusal(409, "conflict"));
    render(<InviteCard {...props} />);
    fireEvent.click(await screen.findByRole("button", { name: "Accept" }));
    fireEvent.click(await screen.findByRole("button", { name: "Reload invitation" }));
    expect((await screen.findByRole("button", { name: "Accept" }) as HTMLButtonElement).disabled).toBe(false);
    expect(client.getBytes).toHaveBeenCalledTimes(2);
  });

  it("keeps the buttons off when a mail send may have gone out", async () => {
    client.postJSON.mockRejectedValueOnce(refusal(409, "kycalendar_off")).mockRejectedValueOnce(new HttpError("request failed: 502", 502, undefined));
    render(<InviteCard {...props} />);
    fireEvent.click(await screen.findByRole("button", { name: "Accept" }));
    fireEvent.click(await screen.findByRole("button", { name: "Send answer" }));
    expect(await screen.findByText(/may have been sent/)).toBeDefined();
    expect((screen.getByRole("button", { name: "Accept" }) as HTMLButtonElement).disabled).toBe(true);
  });

  it("asks again after an update and says what changed", async () => {
    client.postJSON.mockResolvedValue({ outcome: "answered" });
    const first = render(<InviteCard {...props} />);
    fireEvent.click(await screen.findByRole("button", { name: "Accept" }));
    await screen.findByText("Answered through KyCalendar.");
    await vi.waitFor(() => expect(window.localStorage.getItem("kypost.invite-answers.v1")).toContain("ACCEPTED"));
    // Nothing readable is stored: no title, place, address or UID.
    for (const secret of ["Room 1", "alice@example.com", "bob@example.net", "u1", "img"]) {
      expect(window.localStorage.getItem("kypost.invite-answers.v1")).not.toContain(secret);
    }
    first.unmount();

    const again = render(<InviteCard {...props} />);
    expect(await screen.findByText("You accepted.")).toBeDefined();
    again.unmount();

    client.getBytes.mockResolvedValue(new TextEncoder().encode(new TextDecoder().decode(invite())
      .replace("SEQUENCE:1", "SEQUENCE:2").replace("Room 1", "Room 2").replace("T110000Z", "T113000Z")));
    render(<InviteCard {...props} />);
    expect(await screen.findByText("Changed since you answered: time and place. You had accepted; answer again.")).toBeDefined();
    expect((screen.getByRole("button", { name: "Accept" }) as HTMLButtonElement).disabled).toBe(false);
  });

  it("treats a later DTSTAMP at the same SEQUENCE as newer", async () => {
    client.getBytes.mockResolvedValue(invite("REQUEST", "DTSTAMP:20261001T080000Z\r\n"));
    client.postJSON.mockResolvedValue({ outcome: "answered" });
    const first = render(<InviteCard {...props} />);
    fireEvent.click(await screen.findByRole("button", { name: "Decline" }));
    await vi.waitFor(() => expect(window.localStorage.getItem("kypost.invite-answers.v1")).toContain("DECLINED"));
    first.unmount();
    client.getBytes.mockResolvedValue(invite("REQUEST", "DTSTAMP:20261002T080000Z\r\n"));
    render(<InviteCard {...props} />);
    expect(await screen.findByText("This invitation was updated since you answered. You had declined; answer again.")).toBeDefined();
  });

  it("shows a Join button for an https CONFERENCE only", async () => {
    client.getBytes.mockResolvedValue(invite("REQUEST", "CONFERENCE;VALUE=URI;FEATURE=VIDEO;LABEL=KyMessages:https://chat.example.com/#/room/!abc:example\r\n"));
    const { unmount } = render(<InviteCard {...props} />);
    const join = await screen.findByRole("link", { name: /Join meeting/ });
    expect(join.textContent).toBe("Join meeting (chat.example.com)");
    expect(join.getAttribute("href")).toBe("https://chat.example.com/#/room/!abc:example");
    expect(join.getAttribute("target")).toBe("_blank");
    expect(join.getAttribute("rel")).toBe("noopener noreferrer");
    unmount();
    for (const uri of ["javascript:alert(1)", "http://chat.example.com/", "https://user:pw@chat.example.com/", "data:text/html,x"]) {
      client.getBytes.mockResolvedValue(invite("REQUEST", `CONFERENCE;VALUE=URI:${uri}\r\n`));
      const r = render(<InviteCard {...props} />);
      await screen.findByText("Room 1");
      expect(screen.queryByRole("link", { name: /Join meeting/ })).toBeNull();
      r.unmount();
    }
  });

  it("offers no Join on a cancellation", async () => {
    client.getBytes.mockResolvedValue(invite("CANCEL", "CONFERENCE;VALUE=URI:https://chat.example.com/x\r\n"));
    render(<InviteCard {...props} />);
    expect(await screen.findByText("Cancelled")).toBeDefined();
    expect(screen.queryByRole("link", { name: /Join meeting/ })).toBeNull();
  });
});
