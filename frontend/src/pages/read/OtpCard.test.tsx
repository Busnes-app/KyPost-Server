import { afterEach, describe, expect, it } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { OtpCard } from "./OtpCard";

afterEach(cleanup);

const props = {
  subject: "One-time verification code",
  body: "Your verification code is 726343.",
  mode: "plain" as const,
};

describe("OtpCard", () => {
  it("shows a fresh message's code with the never-share warning", () => {
    render(<OtpCard {...props} atUtc={new Date().toISOString()} />);
    expect(screen.getByText("726343")).toBeDefined();
    expect(screen.getByText(/Never share this code/)).toBeDefined();
  });

  it("says so when the clipboard is unavailable", async () => {
    render(<OtpCard {...props} atUtc={new Date().toISOString()} />);
    fireEvent.click(screen.getByRole("button"));
    expect(await screen.findByText("Copy failed, select the code")).toBeDefined();
  });

  it("shows nothing for an old message or one without a code", () => {
    const old = render(<OtpCard {...props} atUtc="2020-01-01T00:00:00Z" />);
    expect(old.container.innerHTML).toBe("");
    const none = render(<OtpCard {...props} body="Lunch on Friday?" subject="Hi" atUtc={new Date().toISOString()} />);
    expect(none.container.innerHTML).toBe("");
  });
});
