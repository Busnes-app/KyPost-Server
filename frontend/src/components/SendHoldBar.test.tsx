import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { SendHoldBar } from "./SendHoldBar";
import type { HeldSend } from "../hooks/useCompose";

afterEach(() => { cleanup(); vi.useRealTimers(); });

const held = (over: Partial<HeldSend> = {}): HeldSend => ({ id: "h", mailbox: "", endsAt: Date.now() + 10_000, draft: {}, from: "", encrypt: false, sign: false, ...over });

it("counts down with Cancel sending and goes when the hold ends", () => {
  vi.useFakeTimers();
  const onCancel = vi.fn();
  const onDismiss = vi.fn();
  render(<SendHoldBar held={held()} onCancel={onCancel} onDismiss={onDismiss} />);
  const bar = screen.getByRole("status");
  expect(bar.textContent).toContain("Sending in 10s");
  expect(bar.textContent?.toLowerCase()).not.toContain("undo");
  fireEvent.click(screen.getByRole("button", { name: "Cancel sending" }));
  expect(onCancel).toHaveBeenCalledTimes(1);
  act(() => vi.advanceTimersByTime(4_000));
  expect(bar.textContent).toContain("Sending in 6s");
  expect(onDismiss).not.toHaveBeenCalled();
  act(() => vi.advanceTimersByTime(6_000));
  expect(onDismiss).toHaveBeenCalledTimes(1);
});

it("waits for a cancel in flight instead of expiring under it", () => {
  vi.useFakeTimers();
  const onDismiss = vi.fn();
  render(<SendHoldBar held={held({ canceling: true })} onCancel={vi.fn()} onDismiss={onDismiss} />);
  expect((screen.getByRole("button", { name: "Cancel sending" }) as HTMLButtonElement).disabled).toBe(true);
  act(() => vi.advanceTimersByTime(11_000));
  expect(onDismiss).not.toHaveBeenCalled();
});

it("after a refused cancel says it is too late and offers no Cancel", () => {
  render(<SendHoldBar held={held({ late: true })} onCancel={vi.fn()} onDismiss={vi.fn()} />);
  expect(screen.getByRole("status").textContent).toContain("Too late to cancel: this message is already being sent.");
  expect(screen.queryByRole("button", { name: "Cancel sending" })).toBeNull();
});
