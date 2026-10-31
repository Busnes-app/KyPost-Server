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
    expect(screen.getByText("This event was cancelled.")).toBeDefined();
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
});
