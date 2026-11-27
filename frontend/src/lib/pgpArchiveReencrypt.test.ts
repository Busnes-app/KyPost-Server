// @vitest-environment node
import { afterEach, beforeAll, expect, it, vi } from "vitest";
import { HttpError } from "../api/client";
import type { PGPBootstrap } from "../api/pgp";
import {
  getPGPReencryptRaw,
  listPGPReencryptCandidates,
  replacePGPReencryptedMessage
} from "../api/pgpReencrypt";
import { lock, unlockWithArmoredKey } from "./keyVault";
import {
  getArchiveReencryptState,
  resetArchiveReencryptState,
  startArchiveReencrypt
} from "./pgpArchiveReencrypt";
import { generateIdentity } from "./pgpClient";
import { pgpEngine, type EngineKey } from "./pgpEngine";
import { addPQCertificate } from "./pgpKeyring";

vi.mock("../api/pgpReencrypt", () => ({
  listPGPReencryptCandidates: vi.fn(),
  getPGPReencryptRaw: vi.fn(),
  replacePGPReencryptedMessage: vi.fn()
}));

let v4: EngineKey;
let v6: EngineKey;
let senderV4: EngineKey;
let foreignV4: EngineKey;
let ringPlaintext: string;
let bootstrap: PGPBootstrap;

beforeAll(async () => {
  const email = "archive@example.test";
  v4 = await generateIdentity("Archive", email);
  v6 = await generateIdentity("Archive", email, undefined, "v6");
  senderV4 = await generateIdentity("Alice", "alice@example.test");
  foreignV4 = await generateIdentity("Stranger", "stranger@example.test");
  const upgraded = await addPQCertificate(
    v4.armoredPrivateKey,
    { fingerprint: v4.fingerprint, publicKey: v4.armoredPublicKey },
    v6
  );
  ringPlaintext = upgraded.plaintext;
  bootstrap = {
    hasIdentity: true,
    protection: "client",
    fingerprint: v4.fingerprint,
    keyId: v4.fingerprint.slice(-16),
    publicKey: v4.armoredPublicKey,
    publicKeyV6: v6.armoredPublicKey,
    fingerprintV6: v6.fingerprint,
    v6Ready: true,
    pgpRevision: 2,
    keySource: "generated",
    createdAt: "2026-11-26T00:00:00Z",
    wrappedPrivateKey: "",
    unlockRequired: false,
    canDecryptServerSide: false,
    migrationAvailable: false,
    signerKeys: [],
    suggestedUserIDs: [email],
    displayName: "Archive"
  };
});

afterEach(() => {
  resetArchiveReencryptState();
  lock();
  vi.clearAllMocks();
});

async function inspectOuterPackets(armored: string): Promise<{ pkeskVersions: number[]; seipdVersion: number }> {
  const bytes = await (await pgpEngine()).dearmor(armored);
  const pkeskVersions: number[] = [];
  let offset = 0;
  while (offset < bytes.length) {
    const header = bytes[offset++];
    if (!(header & 64)) throw new Error("expected new-format OpenPGP packets");
    const tag = header & 63;
    let length = bytes[offset++];
    if (length >= 192 && length < 224) {
      length = ((length - 192) << 8) + bytes[offset++] + 192;
    } else if (length === 255) {
      length = new DataView(bytes.buffer, bytes.byteOffset + offset, 4).getUint32(0);
      offset += 4;
    } else if (length >= 224) {
      if (tag !== 18) throw new Error("unexpected partial-body PKESK");
    }
    if (tag === 1) pkeskVersions.push(bytes[offset]);
    if (tag === 18) return { pkeskVersions, seipdVersion: bytes[offset] };
    offset += length;
  }
  throw new Error("no SEIPD packet found");
}

it("re-encrypts paginated unsigned and inline-signed v4 archive mail to v6 while preserving inner signatures", async () => {
  const engine = await pgpEngine();
  unlockWithArmoredKey(ringPlaintext);

  const unsignedV4 = await engine.encrypt({
    text: "Unsigned archive message",
    encryptionKeys: [v4.armoredPublicKey]
  });
  const signedV4 = await engine.encrypt({
    text: "Inline signed archive message from Alice",
    encryptionKeys: [v4.armoredPublicKey],
    signingKey: senderV4.armoredPrivateKey
  });

  vi.mocked(listPGPReencryptCandidates)
    .mockResolvedValueOnce({
      candidates: [{ folder: "INBOX", messageId: 1, sizeBytes: unsignedV4.length }],
      nextCursor: { folder: "INBOX", afterUid: 1 },
      done: false,
      v6Fingerprint: v6.fingerprint
    })
    .mockResolvedValueOnce({
      candidates: [{ folder: "Sent", messageId: 2, sizeBytes: signedV4.length }],
      done: true,
      v6Fingerprint: v6.fingerprint
    });

  vi.mocked(getPGPReencryptRaw)
    .mockResolvedValueOnce({
      folder: "INBOX",
      messageId: 1,
      sha256: "hash-1",
      armoredCiphertext: unsignedV4,
      sizeBytes: unsignedV4.length
    })
    .mockResolvedValueOnce({
      folder: "Sent",
      messageId: 2,
      sha256: "hash-2",
      armoredCiphertext: signedV4,
      sizeBytes: signedV4.length
    });

  const replacements: Array<{ folder: string; messageId: number; expectedSha256: string; armoredCiphertext: string }> = [];
  vi.mocked(replacePGPReencryptedMessage).mockImplementation(async (req) => {
    replacements.push({
      folder: req.folder,
      messageId: req.messageId,
      expectedSha256: req.expectedSha256,
      armoredCiphertext: req.armoredCiphertext
    });
    return { ok: true, folder: req.folder, oldMessageId: req.messageId, newMessageId: req.messageId };
  });

  const finalState = await startArchiveReencrypt({ bootstrap, userId: "user-1", delayMs: 0, pageSize: 1 });
  expect(finalState).toEqual({ kind: "done", reencrypted: 2, skipped: 0, failed: 0 });
  expect(replacements).toHaveLength(2);
  expect(replacements[0].expectedSha256).toBe("hash-1");
  expect(replacements[1].expectedSha256).toBe("hash-2");

  // Verify every replacement is strictly a single v6 PKESK + SEIPDv2 ciphertext.
  for (const rep of replacements) {
    expect(await inspectOuterPackets(rep.armoredCiphertext)).toEqual({ pkeskVersions: [6], seipdVersion: 2 });
    // Old v4 key must NOT open the re-encrypted message.
    await expect(
      engine.decrypt({
        armoredMessage: rep.armoredCiphertext,
        decryptionKeys: [v4.armoredPrivateKey],
        verificationKeys: [],
        maxBytes: 1 << 20
      })
    ).rejects.toThrow();
  }

  // Unsigned message decrypts with v6 and reports quantumSafe: true.
  const dec1 = await engine.decrypt({
    armoredMessage: replacements[0].armoredCiphertext,
    decryptionKeys: [v6.armoredPrivateKey],
    verificationKeys: [],
    maxBytes: 1 << 20
  });
  expect(dec1.data).toBe("Unsigned archive message");
  expect(dec1.quantumSafe).toBe(true);

  // Inline-signed v4 message decrypts with the [v4, v6] ring and preserves Alice's valid inner signature!
  const dec2 = await engine.decrypt({
    armoredMessage: replacements[1].armoredCiphertext,
    decryptionKeys: [v4.armoredPrivateKey, v6.armoredPrivateKey],
    verificationKeys: [senderV4.armoredPublicKey],
    maxBytes: 1 << 20
  });
  expect(dec2.data).toBe("Inline signed archive message from Alice");
  expect(dec2.quantumSafe).toBe(true);
  expect(dec2.signatures).toHaveLength(1);
  expect(dec2.signatures[0].valid).toBe(true);
});

it("retries once on CAS 409 conflict and skips foreign-key messages without aborting the run", async () => {
  const engine = await pgpEngine();
  unlockWithArmoredKey(ringPlaintext);

  const ownV4 = await engine.encrypt({
    text: "Own message after CAS retry",
    encryptionKeys: [v4.armoredPublicKey]
  });
  const foreignMsg = await engine.encrypt({
    text: "Encrypted to stranger",
    encryptionKeys: [foreignV4.armoredPublicKey]
  });

  vi.mocked(listPGPReencryptCandidates).mockResolvedValueOnce({
    candidates: [
      { folder: "INBOX", messageId: 10, sizeBytes: foreignMsg.length },
      { folder: "INBOX", messageId: 11, sizeBytes: ownV4.length }
    ],
    done: true,
    v6Fingerprint: v6.fingerprint
  });

  vi.mocked(getPGPReencryptRaw)
    .mockResolvedValueOnce({ folder: "INBOX", messageId: 10, sha256: "foreign-hash", armoredCiphertext: foreignMsg, sizeBytes: foreignMsg.length })
    .mockResolvedValueOnce({ folder: "INBOX", messageId: 11, sha256: "stale-hash", armoredCiphertext: ownV4, sizeBytes: ownV4.length })
    .mockResolvedValueOnce({ folder: "INBOX", messageId: 11, sha256: "fresh-hash", armoredCiphertext: ownV4, sizeBytes: ownV4.length });

  vi.mocked(replacePGPReencryptedMessage)
    .mockRejectedValueOnce(new HttpError("request failed: 409 - message changed concurrently", 409, { error: "cas" }))
    .mockResolvedValueOnce({ ok: true, folder: "INBOX", oldMessageId: 11, newMessageId: 11 });

  const finalState = await startArchiveReencrypt({ bootstrap, delayMs: 0 });
  expect(finalState).toEqual({ kind: "incomplete", reencrypted: 1, skipped: 1, failed: 0 });
  expect(replacePGPReencryptedMessage).toHaveBeenCalledTimes(2);
  expect(vi.mocked(replacePGPReencryptedMessage).mock.calls[1][0].expectedSha256).toBe("fresh-hash");
});

it("reports incomplete instead of done when server rejects a replacement with HTTP 400", async () => {
  const engine = await pgpEngine();
  unlockWithArmoredKey(ringPlaintext);

  const ownV4 = await engine.encrypt({
    text: "Message whose replacement is rejected",
    encryptionKeys: [v4.armoredPublicKey]
  });

  vi.mocked(listPGPReencryptCandidates).mockResolvedValueOnce({
    candidates: [{ folder: "INBOX", messageId: 42, sizeBytes: ownV4.length }],
    done: true,
    v6Fingerprint: v6.fingerprint
  });
  vi.mocked(getPGPReencryptRaw).mockResolvedValueOnce({
    folder: "INBOX",
    messageId: 42,
    sha256: "hash-42",
    armoredCiphertext: ownV4,
    sizeBytes: ownV4.length
  });
  vi.mocked(replacePGPReencryptedMessage).mockRejectedValueOnce(
    new HttpError("request failed: 400 - bad ciphertext", 400, { error: "bad ciphertext" })
  );

  const finalState = await startArchiveReencrypt({ bootstrap, delayMs: 0 });
  expect(finalState).toEqual({ kind: "incomplete", reencrypted: 0, skipped: 0, failed: 1 });
});

it("pauses immediately when the vault locks mid-run", async () => {
  const engine = await pgpEngine();
  unlockWithArmoredKey(ringPlaintext);

  const msg = await engine.encrypt({ text: "Message 1", encryptionKeys: [v4.armoredPublicKey] });

  vi.mocked(listPGPReencryptCandidates).mockResolvedValueOnce({
    candidates: [
      { folder: "INBOX", messageId: 1, sizeBytes: msg.length },
      { folder: "INBOX", messageId: 2, sizeBytes: msg.length }
    ],
    done: true,
    v6Fingerprint: v6.fingerprint
  });
  vi.mocked(getPGPReencryptRaw).mockResolvedValue({
    folder: "INBOX",
    messageId: 1,
    sha256: "h1",
    armoredCiphertext: msg,
    sizeBytes: msg.length
  });
  vi.mocked(replacePGPReencryptedMessage).mockImplementation(async () => {
    // User locks vault right after the first replacement finishes.
    lock();
    return { ok: true, folder: "INBOX", oldMessageId: 1, newMessageId: 1 };
  });

  const finalState = await startArchiveReencrypt({ bootstrap, delayMs: 0 });
  expect(finalState).toEqual({ kind: "paused", reason: "locked", reencrypted: 1, skipped: 0, failed: 0 });
  // Second message must never have been fetched or replaced.
  expect(replacePGPReencryptedMessage).toHaveBeenCalledTimes(1);
});

it("does not run when v6Ready is false and pauses when server v6Fingerprint disagrees with local ring", async () => {
  unlockWithArmoredKey(ringPlaintext);

  // 1. Not v6Ready -> stays idle, makes zero API calls.
  await startArchiveReencrypt({ bootstrap: { ...bootstrap, v6Ready: false }, delayMs: 0 });
  expect(getArchiveReencryptState()).toEqual({ kind: "idle" });
  expect(listPGPReencryptCandidates).not.toHaveBeenCalled();

  // 2. Server reports a different v6 fingerprint than our unlocked ring -> pauses safely.
  vi.mocked(listPGPReencryptCandidates).mockResolvedValueOnce({
    candidates: [{ folder: "INBOX", messageId: 1, sizeBytes: 100 }],
    done: true,
    v6Fingerprint: "F".repeat(64)
  });
  const state = await startArchiveReencrypt({ bootstrap, delayMs: 0 });
  expect(state).toEqual({ kind: "paused", reason: "not_ready", reencrypted: 0, skipped: 0, failed: 0 });
  expect(getPGPReencryptRaw).not.toHaveBeenCalled();
});

it("persists continuation cursor when maxPages is reached and resumes from it on the next run", async () => {
  const engine = await pgpEngine();
  unlockWithArmoredKey(ringPlaintext);

  const msg = await engine.encrypt({ text: "Deep archive message", encryptionKeys: [v4.armoredPublicKey] });

  vi.mocked(listPGPReencryptCandidates)
    .mockResolvedValueOnce({
      candidates: [],
      nextCursor: { folder: "Archive", afterUid: 5000 },
      done: false,
      v6Fingerprint: v6.fingerprint
    })
    .mockResolvedValueOnce({
      candidates: [{ folder: "Archive", messageId: 5001, sizeBytes: msg.length }],
      done: true,
      v6Fingerprint: v6.fingerprint
    });
  vi.mocked(getPGPReencryptRaw).mockResolvedValueOnce({
    folder: "Archive",
    messageId: 5001,
    sha256: "hash-5001",
    armoredCiphertext: msg,
    sizeBytes: msg.length
  });
  vi.mocked(replacePGPReencryptedMessage).mockResolvedValueOnce({
    ok: true,
    folder: "Archive",
    oldMessageId: 5001,
    newMessageId: 5001
  });

  const first = await startArchiveReencrypt({ bootstrap, userId: "user-1", delayMs: 0, maxPages: 1 });
  expect(first).toEqual({ kind: "incomplete", reencrypted: 0, skipped: 0, failed: 0, pageLimitReached: true });

  const second = await startArchiveReencrypt({ bootstrap, userId: "user-1", delayMs: 0, maxPages: 1 });
  expect(second).toEqual({ kind: "done", reencrypted: 1, skipped: 0, failed: 0 });
  expect(vi.mocked(listPGPReencryptCandidates).mock.calls[1][0]).toMatchObject({
    folder: "Archive",
    afterUid: 5000
  });
});

it("prevents overlapping concurrent runs started while initialization is in flight", async () => {
  unlockWithArmoredKey(ringPlaintext);

  vi.mocked(listPGPReencryptCandidates).mockResolvedValue({
    candidates: [],
    done: true,
    v6Fingerprint: v6.fingerprint
  });

  // Start two calls back-to-back before async key-ring parsing completes.
  const [first, second] = await Promise.all([
    startArchiveReencrypt({ bootstrap, userId: "user-1", delayMs: 0 }),
    startArchiveReencrypt({ bootstrap, userId: "user-1", delayMs: 0 })
  ]);

  expect(first).toEqual({ kind: "done", reencrypted: 0, skipped: 0, failed: 0 });
  expect(second).toEqual({ kind: "idle" });
  expect(listPGPReencryptCandidates).toHaveBeenCalledTimes(1);
});
