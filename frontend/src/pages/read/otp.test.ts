import { describe, expect, it } from "vitest";
import { extractOtp, isFreshForOtp, OTP_FRESH_MS } from "./otp";

const stateFarm = `<p>Hello MATTHEW,</p>
<p>Here's the code you requested to verify your identity. This code expires in <b>10 minutes</b>.</p>
<p style="color:red"><b>726343</b></p>
<p>If you did not request this, call us at <a href="tel:8007828332">800-782-8332</a>. State Farm® will never contact you for this code.</p>
<p>© 2026 State Farm Mutual Automobile Insurance Company, PO Box 2356</p>`;

describe("extractOtp", () => {
  it("finds the code in a real-world HTML message, not the phone number or year", () => {
    expect(extractOtp("One-time verification code", stateFarm, "html")).toBe("726343");
  });

  it("finds a plain-text code", () => {
    expect(extractOtp("Sign in", "Your verification code is 4821.\nIt expires soon.", "plain")).toBe("4821");
  });

  it("finds a code named only in the subject", () => {
    expect(extractOtp("Your login code", "839201", "plain")).toBe("839201");
  });

  it("ignores numbers in mail that is not about a code", () => {
    expect(extractOtp("Order shipped", "Order 55512345 ships to 1600 Main St. Total $1234.", "plain")).toBe("");
  });

  it("ignores phone numbers, dates, decimals and amounts next to a code word", () => {
    expect(extractOtp("Code", "Call 800-782-8332 on 2026/10/06 about 1234.50 or 9999%", "plain")).toBe("");
  });

  it("finds space- and hyphen-grouped codes and returns them ready to paste", () => {
    expect(extractOtp("Sign in", "Your code is 123 456.", "plain")).toBe("123456");
    expect(extractOtp("Sign in", "Your code is 123-456.", "plain")).toBe("123456");
    expect(extractOtp("Sign in", "Enter code 1234 5678 to continue", "plain")).toBe("12345678");
  });

  it("refuses phone numbers written in groups", () => {
    expect(extractOtp("Code help", "Call 800 782 8332 or 555-1234 or (800) 782-8332", "plain")).toBe("");
  });

  it("finds uppercase alphanumeric codes, keeping their hyphen", () => {
    expect(extractOtp("Verify", "Your verification code: K7P2QX", "plain")).toBe("K7P2QX");
    expect(extractOtp("Verify", "G-123456 is your Google verification code.", "plain")).toBe("G-123456");
    expect(extractOtp("Verify", "Your code is AB3-9XZ2, valid 10 minutes", "plain")).toBe("AB3-9XZ2");
  });

  it("finds a mixed-case code only straight after \"code is\" or \"code:\"", () => {
    expect(extractOtp("Sign in", "Your verification code is a7Bx9k. Ref 123456", "plain")).toBe("a7Bx9k");
    expect(extractOtp("Sign in", "Passcode: q9z2r1", "plain")).toBe("q9z2r1");
    expect(extractOtp("Sign in", "Your code is ready. Tap below.", "plain")).toBe("");
    expect(extractOtp("Sign in", "Enter the code a7Bx9k soon", "plain")).toBe("");
  });

  it("ignores words, short tokens, emails and URL fragments next to a code word", () => {
    expect(extractOtp("Code", "MATTHEW, your 2FA PIN help: ID9@example.com https://x.test/r/AB12CD a_B12C", "plain")).toBe("");
  });

  it("ignores promo codes, order numbers, references and extensions from verified senders", () => {
    expect(extractOtp("Sale", "Use promo code SAVE20 at checkout. Order 2024-ABC", "plain")).toBe("");
    expect(extractOtp("Help", "Enter the code below. Reference ID: AB12-CD34", "plain")).toBe("");
    expect(extractOtp("Security", "We never ask for your PIN. Call ext 4821.", "plain")).toBe("");
  });

  it("finds a 4-digit code only when phrased", () => {
    expect(extractOtp("Sign in", "Your code: 4821", "plain")).toBe("4821");
    expect(extractOtp("Sign in", "Use 4821 to verify", "plain")).toBe("");
  });

  it("ignores CSS inside an HTML body", () => {
    expect(extractOtp("Code", "<style>.code{width:123456px}</style><p>Hello</p>", "html")).toBe("");
  });

  it("stays fast and does not throw on keyword-flooded mail", () => {
    const flood = "code ".repeat(150_000) + " 123456".repeat(2_000);
    const start = performance.now();
    expect(() => extractOtp("code", flood, "plain")).not.toThrow();
    expect(performance.now() - start).toBeLessThan(200);
  });

  it("ignores a number far from any code word", () => {
    expect(extractOtp("Code", `${"x ".repeat(80)}123456`, "plain")).toBe("");
  });
});

describe("isFreshForOtp", () => {
  const now = Date.parse("2026-10-06T10:37:00Z");
  it("accepts a just-delivered message and rejects an old one", () => {
    expect(isFreshForOtp("2026-10-06T10:36:00Z", now)).toBe(true);
    expect(isFreshForOtp(new Date(now - OTP_FRESH_MS).toISOString(), now)).toBe(false);
    expect(isFreshForOtp("not a date", now)).toBe(false);
  });
});
