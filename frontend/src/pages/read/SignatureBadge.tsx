import { signatureLabel, signatureState, type SignatureState } from "./signature";
import type { DecryptedView, InboxEmail } from "./types";

// Unlisted states (checking, unchecked, encrypted but unsigned) are neutral.
const STATUS: Partial<Record<SignatureState, "danger" | "warning" | "success">> = {
  verified: "success",
  conflicted: "warning",
  mismatched: "danger",
};

/**
 * The reading pane's signature badge.
 *
 * Rendered for any signed message, encrypted or not. It used to live inside the
 * encryption badge's conditional, so a signed-but-unencrypted message — the
 * whole population of mail that is authenticated without being secret — got no
 * indicator at all.
 *
 * The fingerprint hint appears only alongside a mismatch, where it is the
 * actionable part: it names the key that actually signed, so the reader can
 * tell an unknown correspondent apart from an impersonation of a known one.
 * Under a pass it would be noise, and under "could not be checked" there is no
 * fingerprint to show.
 */
export function SignatureBadge({
  email,
  local,
  checking
}: {
  email: InboxEmail;
  local?: DecryptedView;
  checking: boolean;
}) {
  const state = signatureState(email, local, checking);
  if (state === "none") {
    return null;
  }
  const fingerprint = local ? local.signerFingerprint : email.pgpSignerFingerprint;
  return (
    <>
      <span
        className="ky-status"
        data-ky-status={STATUS[state]}
        style={{ marginLeft: 6 }}
      >
        {signatureLabel(state)}
      </span>
      {state === "mismatched" && fingerprint ? (
        <span className="contacts-muted" style={{ marginLeft: 6 }}>
          signed by {fingerprint.slice(-16)}
        </span>
      ) : null}
    </>
  );
}
