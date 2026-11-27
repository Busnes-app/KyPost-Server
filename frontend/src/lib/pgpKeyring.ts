// Complete sealed vault plaintext validation. Recovery inspection must never
// install a ring as a committed account update.
import { pgpEngine, type KeyInfo } from "./pgpEngine";

const MAX_KEYRING_BYTES = 512 << 10;
const MAX_KEYS = 16;
const MAX_FINGERPRINTS = 256;

export type KeyringMetadata = {
  version: 1 | 2;
  materialGeneration: number;
  primaryFingerprints: string[];
  keyFingerprints: string[];
};

/** One ring member: its armor exactly as stored, and what the engine read from it. */
export type RingKey = { armored: string; info: KeyInfo; revocationCertificate?: string };

export type ParsedKeyring = {
  activeKey: RingKey;
  activeKeys: RingKey[];
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
      return { kind: "legacy", activeKey, activeKeys: [activeKey], keys: [activeKey] };
    }
    if (raw.length > MAX_KEYRING_BYTES || new TextEncoder().encode(raw).length > MAX_KEYRING_BYTES) invalid();
    const value: unknown = JSON.parse(raw);
    if (!record(value) || (value.format !== "kypost-pgp-keyring-v1" && value.format !== "kypost-pgp-keyring-v2") ||
        typeof value.materialGeneration !== "number" || !Number.isSafeInteger(value.materialGeneration) || value.materialGeneration < 1 ||
        !Array.isArray(value.keyFingerprints) || value.keyFingerprints.length < 1 || value.keyFingerprints.length > MAX_FINGERPRINTS || !value.keyFingerprints.every((item: unknown) => typeof item === "string") ||
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
      keys.push({ ...key, ...(typeof entry.revocationCertificate === "string" ? { revocationCertificate: entry.revocationCertificate } : {}) });
    }
    const inventory = keys.flatMap(key => key.info.fingerprints);
    const declared = value.keyFingerprints.map((fingerprint: string) => fingerprint.toUpperCase());
    if (inventory.length > MAX_FINGERPRINTS || new Set(inventory).size !== inventory.length || new Set(declared).size !== declared.length ||
        inventory.length !== declared.length || !inventory.every(fingerprint => declared.includes(fingerprint))) invalid();
    const activeKeys: RingKey[] = [];
    if (value.format === "kypost-pgp-keyring-v1") {
      if (typeof value.activeFingerprint !== "string") invalid();
      const fingerprint = value.activeFingerprint.toUpperCase();
      const key = keys.find(key => key.info.fingerprint === fingerprint);
      if (!key) invalid();
      activeKeys.push(key);
    } else {
      if (!record(value.active) || "activeFingerprint" in value ||
          Object.keys(value.active).some(version => version !== "v4" && version !== "v6")) invalid();
      for (const [slot, version] of [["v4", 4], ["v6", 6]] as const) {
        const fingerprint = value.active[slot];
        if (fingerprint === undefined) continue;
        if (typeof fingerprint !== "string" || !new RegExp(`^[a-f0-9]{${version === 4 ? 40 : 64}}$`, "i").test(fingerprint)) invalid();
        const key = keys.find(key => key.info.fingerprint === fingerprint.toUpperCase());
        if (!key || key.info.version !== version) invalid();
        activeKeys.push(key);
      }
    }
    // The public identity stays v4; P4 selects a certificate per delivery.
    // Legacy single-key writers still refuse rings.
    const activeKey = activeKeys[0];
    if (!activeKey) invalid();
    const metadata: KeyringMetadata = { version: value.format === "kypost-pgp-keyring-v1" ? 1 : 2, materialGeneration: value.materialGeneration,
      primaryFingerprints: [...primaryFingerprints].sort(), keyFingerprints: inventory.slice().sort() };
    return { kind: "keyring", activeKey, activeKeys, keys, metadata };
  } catch {
    // Parser errors can contain hostile input; never surface key material.
    return invalid();
  }
}

/** Treat server/file metadata as untrusted until all bounds and inventories hold. */
export function parseKeyringMetadata(value: unknown): KeyringMetadata {
  if (!record(value) || (value.version !== 1 && value.version !== 2) || typeof value.materialGeneration !== "number" ||
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
  return { version: value.version, materialGeneration: value.materialGeneration, primaryFingerprints, keyFingerprints };
}

// The public packet multiset: User ID order may differ between the stored
// public key and the ring, so compare packets, not the serialization.
function packetSet(info: KeyInfo): string {
  return JSON.stringify(info.packets.map(p => Array.from(p, b => b.toString(16).padStart(2, "0")).join("")).sort());
}

/** Matches every member and current public packet; matching an active key alone is insufficient. */
export async function validateKeyringSnapshot(raw: string, snapshot: {
  fingerprint: string; publicKey: string; publicKeyV6?: string; keyring: unknown;
}) {
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring") invalid();
  const metadata = parseKeyringMetadata(snapshot.keyring);
  const activeKey = typeof snapshot.fingerprint === "string"
    ? ring.activeKeys.find(key => key.info.fingerprint === snapshot.fingerprint.toUpperCase()) : undefined;
  if (JSON.stringify(metadata) !== JSON.stringify(ring.metadata) || !activeKey) {
    throw new Error("This backup does not contain the current complete keyring. Older backups cannot recover newer keys; keep this copy for a future merge into an unlocked current ring.");
  }
  try {
    if (typeof snapshot.publicKey !== "string" || new TextEncoder().encode(snapshot.publicKey).length > MAX_KEYRING_BYTES) invalid();
    // Both sides are serialized by the same engine, so every User ID,
    // signature and revocation packet must be present in both.
    const key = await (await pgpEngine()).inspect(snapshot.publicKey);
    if (key.count !== 1 || key.isPrivate || packetSet(key) !== packetSet(activeKey.info)) invalid();
    if (snapshot.publicKeyV6) {
      const v6 = ring.activeKeys.find(key => key.info.version === 6);
      const info = await (await pgpEngine()).inspect(snapshot.publicKeyV6);
      if (!v6 || info.isPrivate || info.count !== 1 || packetSet(info) !== packetSet(v6.info)) invalid();
    }
  } catch {
    throw new Error("The backup's public key does not match the current keyring. Public identities or revocations may have changed; no key was restored.");
  }
  return { ...ring, activeKey };
}

/** Add the PQ member without editing, retiring or dropping any existing packet. */
export async function addPQCertificate(raw: string, snapshot: { fingerprint: string; publicKey: string; keyring?: KeyringMetadata | null }, generated: { armoredPrivateKey: string }) {
  const ring = snapshot.keyring ? await validateKeyringSnapshot(raw, { ...snapshot, keyring: snapshot.keyring }) : await parseKeyring(raw);
  const v4 = ring.activeKeys.find(key => key.info.version === 4);
  if (!v4 || v4.info.fingerprint !== snapshot.fingerprint.toUpperCase() || ring.activeKeys.some(key => key.info.version === 6)) invalid();
  const engine = await pgpEngine();
  if (packetSet(await engine.inspect(snapshot.publicKey)) !== packetSet(v4.info)) invalid();
  const pq = await readSingleKey(generated.armoredPrivateKey);
  if (pq.info.version !== 6 || JSON.stringify(pq.info.algorithms) !== "[30,35]") invalid();
  const keys = [...ring.keys, pq];
  const plaintext = JSON.stringify({ format: "kypost-pgp-keyring-v2",
    materialGeneration: ring.kind === "keyring" ? ring.metadata.materialGeneration + 1 : 1,
    active: { v4: v4.info.fingerprint, v6: pq.info.fingerprint },
    keyFingerprints: keys.flatMap(key => key.info.fingerprints),
    keys: keys.map(key => ({ fingerprint: key.info.fingerprint, privateKey: key.armored,
      ...(key.revocationCertificate !== undefined ? { revocationCertificate: key.revocationCertificate } : {}) })) });
  const parsed = await parseKeyring(plaintext);
  if (parsed.kind !== "keyring") invalid();
  return { plaintext, metadata: parsed.metadata };
}
