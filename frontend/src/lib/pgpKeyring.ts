// Complete sealed vault plaintext validation. Conversion stays gated; recovery
// inspection must never install a ring as a committed account update.
import { pgpEngine, type KeyInfo } from "./pgpEngine";

const MAX_KEYRING_BYTES = 128 << 10;
const MAX_KEYS = 16;
const MAX_FINGERPRINTS = 256;

export type KeyringMetadata = {
  version: 1;
  materialGeneration: number;
  primaryFingerprints: string[];
  keyFingerprints: string[];
};

/** One ring member: its armor exactly as stored, and what the engine read from it. */
export type RingKey = { armored: string; info: KeyInfo };

export type ParsedKeyring = {
  activeKey: RingKey;
  keys: RingKey[];
} & ({ kind: "legacy" } | { kind: "keyring"; metadata: KeyringMetadata });

function invalid(): never {
  throw new Error("The PGP keyring is invalid or unsupported. Restore a complete supported backup.");
}

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

async function readSingleKey(armor: string): Promise<RingKey> {
  // OpenPGP armor readers may stop at the first block. Reject trailing blocks
  // rather than silently discarding a second historical key.
  const trimmed = armor.trim();
  if (!trimmed.startsWith("-----BEGIN PGP PRIVATE KEY BLOCK-----") ||
      !trimmed.endsWith("-----END PGP PRIVATE KEY BLOCK-----") ||
      trimmed.split("-----BEGIN PGP ").length !== 2) invalid();
  const info = await (await pgpEngine()).inspect(trimmed);
  // One key per block, and every secret packet decrypted: a ring must not
  // silently accept a still-passphrase-protected historical subkey.
  if (info.count !== 1 || !info.isPrivate || info.locked) invalid();
  return { armored: armor, info };
}

/** Validate the complete payload before exposing any member to a decrypt call. */
export async function parseKeyring(raw: string): Promise<ParsedKeyring> {
  try {
    if (!raw.trimStart().startsWith("{")) {
      // Preserve the existing armor reader: legacy exports may carry preambles,
      // large certification/UID sets or a GNU dummy primary with live subkeys.
      const info = await (await pgpEngine()).inspect(raw);
      if (!info.isPrivate) invalid();
      const activeKey = { armored: raw, info };
      return { kind: "legacy", activeKey, keys: [activeKey] };
    }
    if (raw.length > MAX_KEYRING_BYTES || new TextEncoder().encode(raw).length > MAX_KEYRING_BYTES) invalid();
    const value: unknown = JSON.parse(raw);
    if (!record(value) || value.format !== "kypost-pgp-keyring-v1" ||
        typeof value.activeFingerprint !== "string" ||
        typeof value.materialGeneration !== "number" || !Number.isSafeInteger(value.materialGeneration) || value.materialGeneration < 1 ||
        !Array.isArray(value.keyFingerprints) || !value.keyFingerprints.every((item: unknown) => typeof item === "string") ||
        !Array.isArray(value.keys) || value.keys.length < 1 || value.keys.length > MAX_KEYS) invalid();

    const keys: RingKey[] = [];
    const primaryFingerprints = new Set<string>();
    for (const entry of value.keys) {
      if (!record(entry) || typeof entry.fingerprint !== "string" || typeof entry.privateKey !== "string" ||
          (entry.revocationCertificate !== undefined && typeof entry.revocationCertificate !== "string")) invalid();
      const key = await readSingleKey(entry.privateKey);
      const fingerprint = key.info.fingerprint;
      if (entry.fingerprint.toUpperCase() !== fingerprint || primaryFingerprints.has(fingerprint)) invalid();
      primaryFingerprints.add(fingerprint);
      keys.push(key);
    }
    const inventory = keys.flatMap(key => key.info.fingerprints);
    const declared = value.keyFingerprints.map((fingerprint: string) => fingerprint.toUpperCase());
    const activeFingerprint = value.activeFingerprint.toUpperCase();
    if (inventory.length > MAX_FINGERPRINTS || new Set(inventory).size !== inventory.length || new Set(declared).size !== declared.length ||
        inventory.length !== declared.length || !inventory.every(fingerprint => declared.includes(fingerprint))) invalid();
    const activeKey = keys.find(key => key.info.fingerprint === activeFingerprint);
    if (!activeKey) invalid();
    const metadata: KeyringMetadata = { version: 1, materialGeneration: value.materialGeneration,
      primaryFingerprints: [...primaryFingerprints].sort(), keyFingerprints: inventory.slice().sort() };
    return { kind: "keyring", activeKey, keys, metadata };
  } catch {
    // Parser errors can contain hostile input; never surface key material.
    return invalid();
  }
}

/** Treat server/file metadata as untrusted until all bounds and inventories hold. */
export function parseKeyringMetadata(value: unknown): KeyringMetadata {
  if (!record(value) || value.version !== 1 || typeof value.materialGeneration !== "number" ||
      !Number.isSafeInteger(value.materialGeneration) || value.materialGeneration < 1) invalid();
  function inventory(value: unknown, limit: number): string[] {
    if (!Array.isArray(value) || value.length < 1 || value.length > limit ||
        !value.every((item: unknown): item is string => typeof item === "string" && /^(?:[a-f0-9]{40}|[a-f0-9]{64})$/i.test(item))) invalid();
    const normalized = value.map(item => item.toUpperCase()).sort();
    if (new Set(normalized).size !== normalized.length) invalid();
    return normalized;
  }
  const primaryFingerprints = inventory(value.primaryFingerprints, MAX_KEYS);
  const keyFingerprints = inventory(value.keyFingerprints, MAX_FINGERPRINTS);
  if (!primaryFingerprints.every(fingerprint => keyFingerprints.includes(fingerprint))) invalid();
  return { version: 1, materialGeneration: value.materialGeneration, primaryFingerprints, keyFingerprints };
}

// The public packet multiset: User ID order may differ between the stored
// public key and the ring, so compare packets, not the serialization.
function packetSet(info: KeyInfo): string {
  return JSON.stringify(info.packets.map(p => Array.from(p, b => b.toString(16).padStart(2, "0")).join("")).sort());
}

/** Matches every member and current public packet; matching an active key alone is insufficient. */
export async function validateKeyringSnapshot(raw: string, snapshot: {
  fingerprint: string; publicKey: string; keyring: unknown;
}) {
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring") invalid();
  const metadata = parseKeyringMetadata(snapshot.keyring);
  if (JSON.stringify(metadata) !== JSON.stringify(ring.metadata) ||
      typeof snapshot.fingerprint !== "string" || ring.activeKey.info.fingerprint !== snapshot.fingerprint.toUpperCase()) {
    throw new Error("This backup does not contain the current complete keyring. Older backups cannot recover newer keys; keep this copy for a future merge into an unlocked current ring.");
  }
  try {
    if (typeof snapshot.publicKey !== "string" || new TextEncoder().encode(snapshot.publicKey).length > MAX_KEYRING_BYTES) invalid();
    // Both sides are serialized by the same engine, so every User ID,
    // signature and revocation packet must be present in both.
    const key = await (await pgpEngine()).inspect(snapshot.publicKey);
    if (key.count !== 1 || key.isPrivate || packetSet(key) !== packetSet(ring.activeKey.info)) invalid();
  } catch {
    throw new Error("The backup's public key does not match the current keyring. Public identities or revocations may have changed; no key was restored.");
  }
  return ring;
}
