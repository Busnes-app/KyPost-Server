// pgpSession: the client's PGP state for this page load.
//
// The unwrapped private key cannot survive a reload (see keyVault), so every
// page load is a cold start: fetch the bootstrap snapshot, then prompt for an
// unlock the first time something actually needs the key. This module holds
// that snapshot and the derived "can I do PGP right now" answer, so App,
// SecurityPage, ReadPage, and compose all read one source of truth instead of
// each deciding for themselves and disagreeing.

import {
  getPGPBootstrap,
  getPasswordSnapshot,
  requirePGPRevision,
  type PGPIdentity,
  rewrapPGPPrivateKey,
  rewrapPGPKeyring,
  type BoundSignerKey,
  type PGPBootstrap
} from "../api/pgp";
import {
  isUnlocked,
  lock,
  onVaultChange,
  parseEnvelope,
  requireUnlockedKey,
  requireUnlockedKeyMaterial,
  requireSinglePrivateKey,
  unlock,
  unlockWithArmoredKey,
  unwrapPrivateKey,
  wrapPrivateKey
} from "./keyVault";

import { validateKeyringSnapshot } from "./pgpKeyring";

export type PGPSessionState = {
  loaded: boolean;
  bootstrap: PGPBootstrap | null;
  unlocked: boolean;
  /** Set when the bootstrap fetch itself failed, so callers can say so
   *  rather than rendering "no PGP identity" for a network error. */
  error: string;
};

let state: PGPSessionState = { loaded: false, bootstrap: null, unlocked: false, error: "" };
let sessionGeneration = 0;
let unlockedIdentity: Pick<PGPIdentity, "fingerprint" | "pgpRevision"> | null = null;
const listeners = new Set<(s: PGPSessionState) => void>();

function emit() {
  for (const listener of listeners) {
    listener(state);
  }
}

function setState(patch: Partial<PGPSessionState>) {
  state = { ...state, ...patch };
  emit();
}

// Keep `unlocked` in step with the vault even when something else locks it
// (logout, an explicit lock button).
onVaultChange((unlockedNow) => {
  unlockedIdentity = null;
  if (state.unlocked !== unlockedNow) {
    setState({ unlocked: unlockedNow });
  }
});

export function subscribePGPSession(listener: (s: PGPSessionState) => void): () => void {
  listeners.add(listener);
  listener(state);
  return () => listeners.delete(listener);
}

export function pgpSessionState(): PGPSessionState {
  return state;
}

/**
 * Fetches the cold-start snapshot. Safe to call more than once; later calls
 * refresh it (e.g. after generating a key or migrating).
 */
export async function loadPGPSession(): Promise<PGPSessionState> {
  try {
    const bootstrap = await getPGPBootstrap();
    setState({ loaded: true, bootstrap, unlocked: isUnlocked(), error: "" });
  } catch (e) {
    // A failed bootstrap must not read as "this account has no key" — that
    // is how a client ends up offering to generate a second identity over
    // an existing one.
    setState({ loaded: true, bootstrap: null, error: e instanceof Error ? e.message : "failed to load PGP state" });
  }
  return state;
}

export function clearPGPSession(): void {
  sessionGeneration++;
  lock();
  state = { loaded: false, bootstrap: null, unlocked: false, error: "" };
  emit();
}

/** True when this account holds a key the browser must unwrap itself. */
export function isClientProtected(): boolean {
  return state.bootstrap?.protection === "client";
}

/**
 * Which side holds the key, or "unknown" until the bootstrap has answered.
 * Anything that would write plaintext on a non-client account must branch on
 * this rather than on isClientProtected(): an unloaded or failed bootstrap
 * reads as "not client" there, and a failed fetch is the one condition the
 * server can bring about at will.
 */
export function pgpCustody(): "client" | "other" | "unknown" {
  if (!state.loaded || state.error || !state.bootstrap) return "unknown";
  return state.bootstrap.protection === "client" ? "client" : "other";
}

/**
 * The IMAP account address, which the bootstrap lists first among the key's
 * suggested User IDs. Empty until the bootstrap has loaded.
 */
export function accountAddress(): string {
  return state.bootstrap?.suggestedUserIDs?.[0]?.trim() ?? "";
}

/** True when a PGP operation would need an unlock the user has not done. */
export function needsUnlock(): boolean {
  return isClientProtected() && !state.unlocked;
}

/**
 * Unwraps the stored envelope with password and holds the key for this page.
 * Throws WrongPasswordError from keyVault when the password does not fit.
 */
export async function unlockPGPSession(password: string): Promise<void> {
  const snapshot = state.bootstrap;
  const wrapped = snapshot?.wrappedPrivateKey ?? "";
  const envelope = parseEnvelope(wrapped);
  if (!envelope) {
    throw new Error("No wrapped private key is stored for this account.");
  }
  await unlock(envelope, password);
  unlockedIdentity = snapshot ? { fingerprint: snapshot.fingerprint, pgpRevision: snapshot.pgpRevision } : null;
  setState({ unlocked: true });
}

/** Bind generated/restored plaintext to its confirmed commit, never a later fetch. */
export function acceptCommittedPGPKey(armored: string, identity: Pick<PGPIdentity, "fingerprint" | "pgpRevision">): void {
  unlockWithArmoredKey(armored);
  unlockedIdentity = { fingerprint: identity.fingerprint, pgpRevision: identity.pgpRevision };
}

/** A refreshed bootstrap must never give an older unlocked key a newer revision. */
export function unlockedPGPIdentity(): { fingerprint: string; pgpRevision: number } {
  requireUnlockedKeyMaterial();
  const pgpRevision = requirePGPRevision(unlockedIdentity);
  if (!unlockedIdentity) throw new Error("Reload and unlock the current PGP key.");
  return { fingerprint: unlockedIdentity.fingerprint, pgpRevision };
}

export function lockPGPSession(): void {
  lock();
  setState({ unlocked: false });
}

/**
 * The contact public keys the bootstrap handed over, for verifying inbound
 * signatures, each with the addresses the address book binds it to. Empty is
 * normal (no contacts have keys yet), not an error.
 */
export function knownSignerKeys(): BoundSignerKey[] {
  return state.bootstrap?.signerKeys ?? [];
}

/**
 * Returns the revision and optional re-sealed envelope from one password snapshot.
 * Forced resets preserve the previous envelope for recovery after sign-in.
 *
 * The wrapping key is derived from the account password, so changing the
 * password without re-sealing strands the key: the stored envelope still only
 * opens with the old password, and nothing in the UI would say so.
 *
 * This used to return an uploader the caller invoked AFTER the password write,
 * as a second HTTP request. A dropped connection in between left the password
 * changed and the envelope sealed under a password the user no longer had —
 * permanently, because the only rewrap path re-derives from the CURRENT password
 * and so could never open it again. The advertised recovery ("unlock with your
 * PREVIOUS password, then change your password again") could not work. The
 * envelope is now returned as data and written in the SAME request as the
 * credential, so the pair commits together or not at all.
 */
export async function rewrappedEnvelopeFor(
  oldPassword: string,
  newPassword: string
): Promise<{ expectedRevision: number; rewrappedPgpKey?: string; keyringVersion?: 1 }> {
  const bootstrap = await getPasswordSnapshot();
  const expectedRevision = requirePGPRevision(bootstrap);
  if (bootstrap.mustChangePassword || bootstrap.protection !== "client") return { expectedRevision };
  const envelope = parseEnvelope(bootstrap.wrappedPrivateKey);
  if (!envelope) throw new Error("Your stored PGP envelope cannot be read. Restore it before changing your password.");
  // Unwrapped eagerly, while the old password is known to be correct.
  const raw = await unwrapPrivateKey(envelope, oldPassword);
  if (bootstrap.keyring != null) {
    const current = await getPGPBootstrap();
    if (requirePGPRevision(current) !== expectedRevision || current.protection !== "client" || current.keyring == null) {
      throw new Error("Your PGP state changed. Reload before changing your password.");
    }
    await validateKeyringSnapshot(raw, { ...current, keyring: bootstrap.keyring });
    await validateKeyringSnapshot(raw, { ...current, keyring: current.keyring });
    const rewrappedPgpKey = JSON.stringify(await wrapPrivateKey(raw, newPassword));
    if (new TextEncoder().encode(rewrappedPgpKey).length > 128 << 10) {
      throw new Error("The complete keyring exceeds storage capacity. No password was changed.");
    }
    return { expectedRevision, rewrappedPgpKey, keyringVersion: 1 };
  }
  requireSinglePrivateKey(raw);
  return { expectedRevision, rewrappedPgpKey: JSON.stringify(await wrapPrivateKey(raw, newPassword)) };
}

/**
 * Re-seals the ALREADY-UNLOCKED key under password and uploads it.
 *
 * The recovery path for an envelope that is out of step with the account
 * password — which had no way out at all before: every rewrap derived from the
 * current password, and a stale envelope by definition does not open with it.
 * Unlocking with the older password (PgpUnlockDialog) puts the key in memory,
 * and this writes it back under the current one.
 *
 * `password` does double duty and both uses need it to be the CURRENT account
 * password: it is the key the envelope is re-sealed under, and it is the
 * step-up credential the server now requires before overwriting an envelope it
 * cannot itself inspect.
 */
export async function rewrapUnlockedKeyUnder(password: string): Promise<void> {
  const armored = requireUnlockedKey();
  const snapshot = unlockedPGPIdentity();
  await rewrapPGPPrivateKey(JSON.stringify(await wrapPrivateKey(armored, password)), password, snapshot.fingerprint, snapshot.pgpRevision);
  lockPGPSession();
  await loadPGPSession();
}

/** Confirm the exact prepared write and operation liveness before installing plaintext. */
export async function restorePGPKeyring(raw: string, password: string, snapshot: PGPBootstrap, isActive: () => boolean): Promise<number> {
  const generation = sessionGeneration;
  const isCurrent = () => generation === sessionGeneration && isActive();
  const expectedRevision = requirePGPRevision(snapshot);
  if (snapshot.protection !== "client") throw new Error("This account does not use a complete client keyring.");
  await validateKeyringSnapshot(raw, { ...snapshot, keyring: snapshot.keyring });
  const wrapped = JSON.stringify(await wrapPrivateKey(raw, password));
  if (new TextEncoder().encode(wrapped).length > (128 << 10)) throw new Error("The complete keyring exceeds storage capacity.");
  if (!isCurrent()) throw new Error("The signed-in session changed. Reload before restoring.");
  try {
    await rewrapPGPKeyring({ wrapped, password, expectedFingerprint: snapshot.fingerprint, expectedRevision, isCurrent });
  } catch {
    // The request may have committed. Never retry or attach a newer revision.
  }
  try {
    if (!isCurrent()) throw new Error("session changed");
    const current = await getPGPBootstrap();
    if (requirePGPRevision(current) !== expectedRevision + 1 || current.protection !== "client" || current.wrappedPrivateKey !== wrapped) {
      throw new Error("unconfirmed");
    }
    await validateKeyringSnapshot(raw, { ...current, keyring: current.keyring });
    if (!isCurrent()) throw new Error("session changed");
    const pgpRevision = requirePGPRevision(current);
    acceptCommittedPGPKey(raw, { fingerprint: current.fingerprint, pgpRevision });
    return pgpRevision;
  } catch {
    throw new Error("Restoration could not be confirmed. Keep your recovery file and secret, reload, and try unlocking with your current password before restoring again. No recovered keys were installed in this browser.");
  }
}
