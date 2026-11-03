import { getJSON, putJSON } from "./client";

/** Mirrors backend config.Signature. `text` is plain text, never markup. */
export type Signature = { id: string; name: string; text: string };

/** Mirrors backend config.UserSignatureSettings (GET|PUT /api/mail/signatures). */
export type SignatureSettings = {
  signatures: Signature[];
  /** Used when the From address has no entry in byAddress; "" means none. */
  defaultId: string;
  /** Lowercase address -> signature id; "" there means no signature for that address. */
  byAddress: Record<string, string>;
  onReply: boolean;
  onForward: boolean;
};

export const MAX_SIGNATURES = 10;
export const MAX_SIGNATURE_NAME = 60;
export const MAX_SIGNATURE_TEXT = 2000;

export function getSignatures(): Promise<SignatureSettings> {
  return getJSON<SignatureSettings>("/api/mail/signatures");
}

export function putSignatures(settings: SignatureSettings): Promise<SignatureSettings> {
  return putJSON<SignatureSettings>("/api/mail/signatures", settings);
}
