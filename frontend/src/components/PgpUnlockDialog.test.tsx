import { afterEach, expect, it, vi } from "vitest";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { PgpUnlockDialog } from "./PgpUnlockDialog";
import { unlockPGPSession } from "../lib/pgpSession";

vi.mock("../lib/pgpSession", () => ({ unlockPGPSession: vi.fn() }));

afterEach(cleanup);

// Read and compose both mount this dialog; a shared id sent a label (and the
// launch-video harness) to the other dialog's hidden input.
it("gives each mounted dialog its own password input id", () => {
  render(
    <>
      <PgpUnlockDialog open reason="to read this message" onUnlocked={() => {}} onCancel={() => {}} />
      <PgpUnlockDialog open={false} reason="to send this message" onUnlocked={() => {}} onCancel={() => {}} />
    </>
  );
  const inputs = document.querySelectorAll<HTMLInputElement>('input[type="password"]');
  expect(inputs).toHaveLength(2);
  expect(inputs[0].id).not.toBe(inputs[1].id);
  expect(screen.getAllByLabelText("Account password", { selector: "input" })).toEqual([...inputs]);
});

// Escape cancels while the key derivation is still running. The caller has
// moved on by the time it finishes, so the late unlock must call nobody.
it("a cancelled unlock that finishes later does not report success", async () => {
  let finish!: () => void;
  vi.mocked(unlockPGPSession).mockReturnValueOnce(new Promise<never>((resolve) => { finish = resolve as () => void; }));
  const onUnlocked = vi.fn();
  const onCancel = vi.fn();
  render(<PgpUnlockDialog open reason="to send this message" onUnlocked={onUnlocked} onCancel={onCancel} />);
  fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "pw" } });
  fireEvent.click(screen.getByRole("button", { name: "Unlock" }));
  fireEvent(document.querySelector("dialog")!, new Event("cancel", { cancelable: true }));
  expect(onCancel).toHaveBeenCalledTimes(1);
  await act(async () => { finish(); });
  expect(onUnlocked).not.toHaveBeenCalled();
});

it("an unlock that is not cancelled reports success", async () => {
  vi.mocked(unlockPGPSession).mockResolvedValueOnce(undefined as never);
  const onUnlocked = vi.fn();
  render(<PgpUnlockDialog open reason="to send this message" onUnlocked={onUnlocked} onCancel={() => {}} />);
  fireEvent.change(screen.getByLabelText("Account password"), { target: { value: "pw" } });
  await act(async () => { fireEvent.click(screen.getByRole("button", { name: "Unlock" })); });
  expect(onUnlocked).toHaveBeenCalledTimes(1);
});
