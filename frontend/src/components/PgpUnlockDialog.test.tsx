import { afterEach, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { PgpUnlockDialog } from "./PgpUnlockDialog";

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
