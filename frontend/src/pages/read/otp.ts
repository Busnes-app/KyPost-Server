// Verification-code detection for the reader's copy card. Pure pattern
// matching, never the classifier: a model reading hostile mail can be told
// what to answer, and this runs on every recent message the reader opens.

/** How long after delivery the card is offered; these codes expire in minutes. */
export const OTP_FRESH_MS = 15 * 60 * 1000;

// Hostile mail can be megabytes of keywords; a code sits near the top.
const MAX_HTML = 256 * 1024;
const MAX_TEXT = 8 * 1024;

const WORD = String.raw`(?:code|passcode|otp|pin)`;
// Edges shared by every candidate: not inside a word, phone number, date,
// amount, URL, email address or a longer run of digit groups.
const BEFORE = String.raw`(?<![\w\-./,:$#@]|\d[ -])`;
const AFTER = String.raw`(?![\w\-/:%@]|[.,]\w|[ -]\d)`;
// Any code shape: letters and digits with at most one hyphen, or two equal
// digit groups. Must contain a digit.
const TOKEN = String.raw`(?=[A-Za-z0-9 -]*\d)(?:\d{3}[ -]\d{3}|\d{4}[ -]\d{4}|[A-Za-z0-9]+(?:-[A-Za-z0-9]+)?)`;
// "code is X", "code: X", "X is your ... code". The only path for 4-digit and
// letter codes: near-miss proximity there matched promo codes, order numbers
// and phone extensions.
const PHRASED = [
  new RegExp(String.raw`\b${WORD}(?: is|:)\s+${BEFORE}(${TOKEN})${AFTER}`, "i"),
  new RegExp(String.raw`${BEFORE}(${TOKEN})${AFTER} is your [\w .-]{0,40}?\b${WORD}\b`, "i"),
];
// Without a phrase, only 5-8 digits or equal digit groups near a code word.
const KEYWORD = /\b(?:code|passcode|one[- ]time|otp|verification|verify|pin)\b/gi;
const NUMERIC = new RegExp(`${BEFORE}(?:\\d{5,8}|\\d{3}[ -]\\d{3}|\\d{4}[ -]\\d{4})${AFTER}`, "g");
// ponytail: proximity heuristic, no layout awareness. Among numbers within
// MAX_DISTANCE of a code word the longest wins, then the nearest, so a 6-digit
// code beats a footer PO box. O(keywords x numbers), bounded by MAX_TEXT.
const MAX_DISTANCE = 100;

function textOf(body: string, mode: "html" | "plain" | undefined): string {
  if (mode !== "html") return body.slice(0, MAX_TEXT);
  const doc = new DOMParser().parseFromString(body.slice(0, MAX_HTML), "text/html");
  doc.querySelectorAll("style, script, title").forEach((el) => el.remove());
  return (doc.body.textContent ?? "").replace(/\s+/g, " ").slice(0, MAX_TEXT);
}

// Digit groups come back without their separator, ready to paste; letter
// codes keep their hyphen, which may be part of the code.
const normalize = (code: string) => (/^[\d -]+$/.test(code) ? code.replace(/[ -]/g, "") : code);

/** Returns the verification code in subject or body, or "". */
export function extractOtp(subject: string, body: string, mode: "html" | "plain" | undefined): string {
  const text = `${subject.slice(0, 512)}\n${textOf(body, mode)}`;
  for (const re of PHRASED) {
    const m = re.exec(text);
    if (m && m[1].replace(/[ -]/g, "").length >= 4 && m[1].length <= 12) return normalize(m[1]);
  }
  const keywords = [...text.matchAll(KEYWORD)].map((m) => m.index);
  let best = "";
  let bestDistance = Infinity;
  for (const m of text.matchAll(NUMERIC)) {
    const code = normalize(m[0]);
    let distance = Infinity;
    for (const k of keywords) distance = Math.min(distance, Math.abs(m.index - k));
    if (distance <= MAX_DISTANCE && (code.length > best.length || (code.length === best.length && distance < bestDistance))) {
      best = code;
      bestDistance = distance;
    }
  }
  return best;
}

export function isFreshForOtp(atUtc: string, now = Date.now()): boolean {
  const at = Date.parse(atUtc);
  return Number.isFinite(at) && now - at >= -60_000 && now - at < OTP_FRESH_MS;
}
