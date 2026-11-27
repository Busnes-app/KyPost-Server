import { HttpError, SessionExpiredError } from "../api/client";
import type { PGPBootstrap } from "../api/pgp";
import {
  getPGPReencryptRaw,
  listPGPReencryptCandidates,
  replacePGPReencryptedMessage,
  type PGPReencryptCandidate,
  type PGPReencryptCursor
} from "../api/pgpReencrypt";
import { isUnlocked, onVaultChange, requireUnlockedKeyMaterial } from "./keyVault";
import { pgpEngine, type PgpEngine } from "./pgpEngine";
import { parseKeyring } from "./pgpKeyring";
import { pgpSessionState, subscribePGPSession } from "./pgpSession";

const DEFAULT_PAGE_SIZE = 25;
const DEFAULT_DELAY_MS = 150;
// Matches backend mailmsg.MaxInboundMessageBytes (25 MiB).
const DEFAULT_MAX_BYTES = 25 * 1024 * 1024;
// Hard cap on cursor pages per run to guard against a broken server cursor loop.
const MAX_PAGES_PER_RUN = 10_000;

export type ArchiveReencryptPauseReason = "locked" | "aborted" | "not_ready";

export type ArchiveReencryptState =
  | { kind: "idle" }
  | { kind: "running"; reencrypted: number; skipped: number; failed: number }
  | { kind: "done"; reencrypted: number; skipped: 0; failed: 0 }
  | { kind: "incomplete"; reencrypted: number; skipped: number; failed: number; pageLimitReached?: boolean }
  | { kind: "paused"; reason: ArchiveReencryptPauseReason; reencrypted: number; skipped: number; failed: number }
  | { kind: "error"; message: string; reencrypted: number; skipped: number; failed: number };

export type ArchiveReencryptOptions = {
  /** Override bootstrap snapshot (defaults to current pgpSessionState().bootstrap). */
  bootstrap?: PGPBootstrap | null;
  /** Optional user ID attached via X-KyPost-User for cross-account safety. */
  userId?: string;
  /** Candidates requested per cursor page. Defaults to 25. */
  pageSize?: number;
  /** Throttle delay between messages in milliseconds. Defaults to 150ms. */
  delayMs?: number;
  /** Maximum plaintext bytes allowed by WASM decryption/re-encryption. */
  maxBytes?: number;
  /** Maximum cursor pages inspected per run. Defaults to 10,000. */
  maxPages?: number;
  /** Optional engine override for tests. */
  engine?: PgpEngine;
};

type SavedResumeCursor = {
  key: string;
  cursor: PGPReencryptCursor;
  priorSkipped: number;
  priorFailed: number;
};

let currentState: ArchiveReencryptState = { kind: "idle" };
let activeRunId = 0;
let activeController: AbortController | null = null;
let savedResumeCursor: SavedResumeCursor | null = null;
const listeners = new Set<(state: ArchiveReencryptState) => void>();

function resumeScopeKey(userId: string | undefined, v6Fingerprint: string): string {
  return `${userId ?? ""}|${v6Fingerprint.toUpperCase()}`;
}

function emit(next: ArchiveReencryptState): void {
  currentState = next;
  for (const listener of listeners) {
    listener(next);
  }
}

export function getArchiveReencryptState(): ArchiveReencryptState {
  return currentState;
}

export function subscribeArchiveReencrypt(listener: (state: ArchiveReencryptState) => void): () => void {
  listeners.add(listener);
  listener(currentState);
  return () => {
    listeners.delete(listener);
  };
}

/** Cancels any active archive re-encryption run immediately. */
export function stopArchiveReencrypt(reason: ArchiveReencryptPauseReason = "aborted"): void {
  if (activeController) {
    const controller = activeController;
    activeController = null;
    activeRunId++;
    controller.abort();
    if (currentState.kind === "running") {
      emit({
        kind: "paused",
        reason,
        reencrypted: currentState.reencrypted,
        skipped: currentState.skipped,
        failed: currentState.failed
      });
    }
  }
}

/** Resets state to idle (used on logout or test teardown). */
export function resetArchiveReencryptState(): void {
  stopArchiveReencrypt("aborted");
  savedResumeCursor = null;
  emit({ kind: "idle" });
}

// Immediately stop any in-flight run if the private key vault locks.
onVaultChange((unlocked) => {
  if (!unlocked) {
    stopArchiveReencrypt("locked");
  }
});

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  if (ms <= 0 || signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const onAbort = () => {
      clearTimeout(timer);
      resolve();
    };
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

type RingMaterials = {
  decryptionKeys: string[];
  encryptionKey: string;
  v6Fingerprint: string;
};

async function loadUnlockedV6Ring(bootstrap: PGPBootstrap, engine: PgpEngine): Promise<RingMaterials | null> {
  if (!isUnlocked()) return null;
  let raw: string;
  try {
    raw = requireUnlockedKeyMaterial();
  } catch {
    return null;
  }
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring" || ring.metadata.version !== 2) {
    return null;
  }
  const activeV6 = ring.activeKeys.find((k) => k.info.version === 6 && k.info.encryptionAlgorithm === 35);
  if (!activeV6) {
    return null;
  }
  const expectedV6Fp = activeV6.info.fingerprint.toUpperCase();
  if (bootstrap.fingerprintV6 && bootstrap.fingerprintV6.toUpperCase() !== expectedV6Fp) {
    return null;
  }
  let encryptionKey = activeV6.armored;
  if (bootstrap.publicKeyV6) {
    const pubInfo = await engine.inspect(bootstrap.publicKeyV6);
    if (pubInfo.count !== 1 || pubInfo.isPrivate || pubInfo.version !== 6 || pubInfo.encryptionAlgorithm !== 35 ||
        pubInfo.fingerprint.toUpperCase() !== expectedV6Fp) {
      return null;
    }
    encryptionKey = bootstrap.publicKeyV6;
  }
  const decryptionKeys = ring.keys.map((k) => k.armored);
  if (decryptionKeys.length === 0) return null;
  return {
    decryptionKeys,
    encryptionKey,
    v6Fingerprint: expectedV6Fp
  };
}

type CandidateOutcome = "reencrypted" | "skipped" | "failed" | "aborted";

async function reencryptCandidateOnce(opts: {
  candidate: PGPReencryptCandidate;
  materials: RingMaterials;
  engine: PgpEngine;
  maxBytes: number;
  userId?: string;
  signal: AbortSignal;
}): Promise<CandidateOutcome> {
  const { candidate, materials, engine, maxBytes, userId, signal } = opts;
  // Retry once on CAS conflict (409 on replace) in case flags/state changed concurrently.
  for (let attempt = 0; attempt < 2; attempt++) {
    if (signal.aborted || !isUnlocked()) return "aborted";
    let raw;
    try {
      raw = await getPGPReencryptRaw({
        folder: candidate.folder,
        messageId: candidate.messageId,
        userId
      });
    } catch (err) {
      if (err instanceof SessionExpiredError) return "aborted";
      if (err instanceof HttpError && (err.status === 404 || err.status === 409)) {
        // Deleted, moved, or already upgraded to v6.
        return "skipped";
      }
      throw err;
    }
    if (signal.aborted || !isUnlocked()) return "aborted";

    let v6Ciphertext: string;
    try {
      v6Ciphertext = await engine.reencrypt({
        armoredMessage: raw.armoredCiphertext,
        decryptionKeys: materials.decryptionKeys,
        encryptionKey: materials.encryptionKey,
        maxBytes
      });
    } catch {
      // Encrypted to a key not in this ring, corrupt ciphertext, or over maxBytes:
      // skip without failing the rest of the mailbox.
      return "skipped";
    }
    if (signal.aborted || !isUnlocked()) return "aborted";

    try {
      await replacePGPReencryptedMessage({
        folder: candidate.folder,
        messageId: candidate.messageId,
        expectedSha256: raw.sha256,
        armoredCiphertext: v6Ciphertext,
        userId
      });
      return "reencrypted";
    } catch (err) {
      if (err instanceof SessionExpiredError) return "aborted";
      if (err instanceof HttpError && err.status === 404) {
        return "skipped";
      }
      if (err instanceof HttpError && err.status === 409) {
        // CAS mismatch or already replaced by another client: loop once to re-check /raw.
        if (attempt === 0) continue;
        return "skipped";
      }
      if (err instanceof HttpError && err.status === 400) {
        return "failed";
      }
      throw err;
    }
  }
  return "skipped";
}

/**
 * Runs a single resumable pass re-encrypting stored v4 messages in the user's
 * primary mailbox to their active v6 PQ key.
 *
 * No-ops cleanly if already running, if the account is not client-protected +
 * v6Ready, or if the local vault is locked.
 */
export async function startArchiveReencrypt(options: ArchiveReencryptOptions = {}): Promise<ArchiveReencryptState> {
  if (activeController) {
    return currentState;
  }

  const bootstrap = options.bootstrap !== undefined ? options.bootstrap : pgpSessionState().bootstrap;
  if (!bootstrap || bootstrap.protection !== "client" || bootstrap.v6Ready !== true || !isUnlocked()) {
    return currentState;
  }

  // Reserve the single-run slot synchronously before any async initialization
  // so overlapping calls cannot race past the guard and leak an untracked run.
  const runId = ++activeRunId;
  const controller = new AbortController();
  activeController = controller;
  const signal = controller.signal;
  const pageSize = options.pageSize ?? DEFAULT_PAGE_SIZE;
  const delayMs = options.delayMs ?? DEFAULT_DELAY_MS;
  const maxBytes = options.maxBytes ?? DEFAULT_MAX_BYTES;
  const maxPages = Math.max(1, options.maxPages ?? MAX_PAGES_PER_RUN);
  const userId = options.userId;

  let reencrypted = 0;
  let skipped = 0;
  let failed = 0;

  const isStillCurrent = () => activeRunId === runId && !signal.aborted && isUnlocked();
  const emitRunning = () => {
    if (isStillCurrent()) {
      emit({ kind: "running", reencrypted, skipped, failed });
    }
  };
  const finish = (next: ArchiveReencryptState): ArchiveReencryptState => {
    if (activeController === controller) {
      activeController = null;
    }
    if (activeRunId === runId || (activeController === null && next.kind === "paused" && currentState.kind === "paused")) {
      emit(next);
    }
    return currentState;
  };

  try {
    const engine = options.engine ?? (await pgpEngine());
    if (!isStillCurrent()) {
      return finish({
        kind: "paused",
        reason: isUnlocked() ? "aborted" : "locked",
        reencrypted,
        skipped,
        failed
      });
    }
    const materials = await loadUnlockedV6Ring(bootstrap, engine);
    if (!isStillCurrent()) {
      return finish({
        kind: "paused",
        reason: isUnlocked() ? "aborted" : "locked",
        reencrypted,
        skipped,
        failed
      });
    }
    if (!materials) {
      if (activeController === controller) {
        activeController = null;
      }
      return currentState;
    }

    const scopeKey = resumeScopeKey(userId, materials.v6Fingerprint);
    emit({ kind: "running", reencrypted, skipped, failed });

    const resumed = savedResumeCursor && savedResumeCursor.key === scopeKey ? savedResumeCursor : null;
    let cursor: PGPReencryptCursor | undefined = resumed?.cursor;
    const priorSkipped = resumed?.priorSkipped ?? 0;
    const priorFailed = resumed?.priorFailed ?? 0;
    for (let page = 0; page < maxPages; page++) {
      if (!isStillCurrent()) {
        return finish({
          kind: "paused",
          reason: isUnlocked() ? "aborted" : "locked",
          reencrypted,
          skipped,
          failed
        });
      }

      let listing;
      try {
        listing = await listPGPReencryptCandidates({
          folder: cursor?.folder,
          afterUid: cursor?.afterUid,
          limit: pageSize,
          userId
        });
      } catch (err) {
        if (err instanceof SessionExpiredError) {
          return finish({ kind: "paused", reason: "aborted", reencrypted, skipped, failed });
        }
        if (err instanceof HttpError && err.status === 409) {
          // Server reports account is no longer v6Ready (e.g. legacy device enrolled).
          return finish({ kind: "paused", reason: "not_ready", reencrypted, skipped, failed });
        }
        throw err;
      }

      if (!isStillCurrent()) {
        return finish({
          kind: "paused",
          reason: isUnlocked() ? "aborted" : "locked",
          reencrypted,
          skipped,
          failed
        });
      }

      if (listing.v6Fingerprint && listing.v6Fingerprint.toUpperCase() !== materials.v6Fingerprint) {
        // Keyring rotated or out of sync with server's active v6 key: refuse to write.
        return finish({ kind: "paused", reason: "not_ready", reencrypted, skipped, failed });
      }

      for (const candidate of listing.candidates) {
        if (!isStillCurrent()) {
          return finish({
            kind: "paused",
            reason: isUnlocked() ? "aborted" : "locked",
            reencrypted,
            skipped,
            failed
          });
        }
        const outcome = await reencryptCandidateOnce({
          candidate,
          materials,
          engine,
          maxBytes,
          userId,
          signal
        });
        switch (outcome) {
          case "reencrypted":
            reencrypted++;
            emitRunning();
            break;
          case "skipped":
            skipped++;
            emitRunning();
            break;
          case "failed":
            failed++;
            emitRunning();
            break;
          case "aborted":
            return finish({
              kind: "paused",
              reason: isUnlocked() ? "aborted" : "locked",
              reencrypted,
              skipped,
              failed
            });
          default: {
            const _exhaustive: never = outcome;
            return _exhaustive;
          }
        }
        if (delayMs > 0) {
          await sleep(delayMs, signal);
        }
      }

      if (listing.done || !listing.nextCursor) {
        if (savedResumeCursor?.key === scopeKey) {
          savedResumeCursor = null;
        }
        const totalSkipped = priorSkipped + skipped;
        const totalFailed = priorFailed + failed;
        if (totalSkipped > 0 || totalFailed > 0) {
          return finish({ kind: "incomplete", reencrypted, skipped: totalSkipped, failed: totalFailed });
        }
        return finish({ kind: "done", reencrypted, skipped: 0, failed: 0 });
      }
      cursor = listing.nextCursor;
      savedResumeCursor = {
        key: scopeKey,
        cursor,
        priorSkipped: priorSkipped + skipped,
        priorFailed: priorFailed + failed
      };
    }

    return finish({
      kind: "incomplete",
      reencrypted,
      skipped: priorSkipped + skipped,
      failed: priorFailed + failed,
      pageLimitReached: true
    });
  } catch (err) {
    if (err instanceof SessionExpiredError || signal.aborted) {
      return finish({
        kind: "paused",
        reason: isUnlocked() ? "aborted" : "locked",
        reencrypted,
        skipped,
        failed
      });
    }
    const message = err instanceof Error ? err.message : "archive re-encryption failed";
    return finish({ kind: "error", message, reencrypted, skipped, failed });
  }
}

/**
 * Watches pgpSession state and automatically kicks off a resumable archive
 * re-encryption pass whenever the user unlocks a client-protected, v6-ready
 * account. Stops immediately on lock or logout.
 */
export function attachArchiveReencryptWatcher(getUserId?: () => string | undefined): () => void {
  let triggeredKey = "";
  return subscribePGPSession((session) => {
    const ready =
      session.loaded &&
      !session.error &&
      session.unlocked &&
      session.bootstrap?.protection === "client" &&
      session.bootstrap.v6Ready === true;

    if (!ready || !session.bootstrap) {
      triggeredKey = "";
      if (!session.loaded && !session.bootstrap) {
        resetArchiveReencryptState();
      } else {
        stopArchiveReencrypt(session.unlocked ? "not_ready" : "locked");
      }
      return;
    }

    const userId = getUserId?.();
    const key = `${userId ?? ""}|${session.bootstrap.fingerprintV6 ?? ""}|${session.bootstrap.pgpRevision ?? 0}`;
    if (triggeredKey === key) return;
    triggeredKey = key;
    void startArchiveReencrypt({ bootstrap: session.bootstrap, userId }).catch(() => {
      // Errors are surfaced through ArchiveReencryptState; never leave an unhandled rejection.
    });
  });
}
