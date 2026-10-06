import { useMemo, useState } from "react";
import { extractOtp, isFreshForOtp } from "./otp";

type Props = {
  subject: string;
  body: string;
  mode: "html" | "plain" | undefined;
  atUtc: string;
};

// OtpCard offers a recent message's verification code for copying. It claims
// nothing about the sender: the code is the message's own text, so a forged
// one gains nothing, and checking the sender would cost a DNS lookup the
// sender can watch as a read receipt. Render it keyed by message so state
// never carries over to another message.
export function OtpCard({ subject, body, mode, atUtc }: Props) {
  const code = useMemo(() => (isFreshForOtp(atUtc) ? extractOtp(subject, body, mode) : ""), [subject, body, mode, atUtc]);
  const [copyLabel, setCopyLabel] = useState("Copy code");

  if (!code) return null;
  return (
    <section className="otp-card" aria-label="Verification code">
      <code className="otp-card-code">{code}</code>
      <button
        type="button"
        onClick={() => {
          // Clipboard is absent outside a secure context; the code stays selectable.
          Promise.resolve(navigator.clipboard?.writeText(code) ?? Promise.reject()).then(
            () => setCopyLabel("Copied"),
            () => setCopyLabel("Copy failed, select the code")
          );
        }}
      >
        {copyLabel}
      </button>
      <span className="otp-card-warning">Never share this code with anyone who contacts you.</span>
    </section>
  );
}
