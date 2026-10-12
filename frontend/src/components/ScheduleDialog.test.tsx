import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ScheduleDialog } from "./ScheduleDialog";
import { localDateTimeValue } from "../lib/localDateTime";

afterEach(cleanup);

describe("ScheduleDialog", () => {
  it("schedules a preset", async () => {
    const onSchedule = vi.fn();
    render(<ScheduleDialog open onSchedule={onSchedule} onCancel={() => {}} />);
    const before = Date.now();
    await userEvent.click(screen.getByRole("button", { name: /In 1 hour/ }));
    const at: Date = onSchedule.mock.calls[0][0];
    expect(at.getTime()).toBeGreaterThan(before + 58 * 60_000);
    expect(at.getTime()).toBeLessThanOrEqual(before + 60 * 60_000);
  });

  it("refuses a custom time in the past or over a year away", () => {
    render(<ScheduleDialog open onSchedule={() => {}} onCancel={() => {}} />);
    const input = screen.getByLabelText("Pick a date and time");
    const schedule = screen.getByRole<HTMLButtonElement>("button", { name: "Schedule" });
    for (const at of [new Date(Date.now() - 60_000), new Date(Date.now() + 400 * 86_400_000)]) {
      fireEvent.change(input, { target: { value: localDateTimeValue(at) } });
      expect(screen.getByText(/between one minute and a year/)).toBeTruthy();
      expect(schedule.disabled).toBe(true);
    }
  });

  it("schedules a custom time", async () => {
    const onSchedule = vi.fn();
    render(<ScheduleDialog open onSchedule={onSchedule} onCancel={() => {}} />);
    const at = new Date(Date.now() + 3 * 86_400_000);
    at.setSeconds(0, 0);
    fireEvent.change(screen.getByLabelText("Pick a date and time"), { target: { value: localDateTimeValue(at) } });
    await userEvent.click(screen.getByRole("button", { name: "Schedule" }));
    expect(onSchedule).toHaveBeenCalledWith(at);
  });

  it("cancels", async () => {
    const onCancel = vi.fn();
    render(<ScheduleDialog open onSchedule={() => {}} onCancel={onCancel} />);
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(onCancel).toHaveBeenCalled();
  });
});
