import { getJSON, mailboxHeaders, postJSON } from "./client";

export type PGPReencryptCandidate = {
  folder: string;
  messageId: number;
  sizeBytes: number;
};

export type PGPReencryptCursor = {
  folder: string;
  afterUid: number;
};

export type PGPReencryptCandidatesResponse = {
  candidates: PGPReencryptCandidate[];
  nextCursor?: PGPReencryptCursor;
  done: boolean;
  skippedOversized?: number;
  v6Fingerprint: string;
};

export type PGPReencryptRawResponse = {
  folder: string;
  messageId: number;
  sha256: string;
  armoredCiphertext: string;
  sizeBytes: number;
};

export type PGPReencryptReplaceResponse = {
  ok: boolean;
  folder: string;
  oldMessageId: number;
  newMessageId: number;
};

function reencryptHeaders(userId?: string): Record<string, string> {
  // Always pin archive re-encryption to the user's primary mailbox so switching
  // webmail tabs to a secondary/shared mailbox mid-job cannot retarget writes.
  return {
    ...mailboxHeaders(""),
    ...(userId ? { "X-KyPost-User": userId } : {})
  };
}

export function listPGPReencryptCandidates(opts: {
  folder?: string;
  afterUid?: number;
  limit?: number;
  userId?: string;
}): Promise<PGPReencryptCandidatesResponse> {
  const params = new URLSearchParams();
  if (opts.folder) params.set("folder", opts.folder);
  if (opts.afterUid && opts.afterUid > 0) params.set("afterUid", String(opts.afterUid));
  if (opts.limit && opts.limit > 0) params.set("limit", String(opts.limit));
  const query = params.toString();
  const path = query ? `/api/mail/pgp-reencrypt/candidates?${query}` : "/api/mail/pgp-reencrypt/candidates";
  return getJSON<PGPReencryptCandidatesResponse>(path, reencryptHeaders(opts.userId));
}

export function getPGPReencryptRaw(opts: {
  folder: string;
  messageId: number;
  userId?: string;
}): Promise<PGPReencryptRawResponse> {
  const params = new URLSearchParams({
    folder: opts.folder,
    messageId: String(opts.messageId)
  });
  return getJSON<PGPReencryptRawResponse>(`/api/mail/pgp-reencrypt/raw?${params.toString()}`, reencryptHeaders(opts.userId));
}

export function replacePGPReencryptedMessage(opts: {
  folder: string;
  messageId: number;
  expectedSha256: string;
  armoredCiphertext: string;
  userId?: string;
}): Promise<PGPReencryptReplaceResponse> {
  return postJSON<PGPReencryptReplaceResponse>(
    "/api/mail/pgp-reencrypt/replace",
    {
      folder: opts.folder,
      messageId: opts.messageId,
      expectedSha256: opts.expectedSha256,
      armoredCiphertext: opts.armoredCiphertext
    },
    reencryptHeaders(opts.userId)
  );
}
