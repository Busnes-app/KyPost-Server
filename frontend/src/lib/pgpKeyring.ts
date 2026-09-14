// Complete sealed vault plaintext validation. Conversion stays gated; recovery
// inspection must never install a ring as a committed account update.
import type { Message, PrivateKey, PublicKey } from "openpgp";

const MAX_KEYRING_BYTES = 128 << 10;
const MAX_KEYS = 16;
const MAX_FINGERPRINTS = 256;

export type KeyringMetadata = {
  version: 1;
  materialGeneration: number;
  primaryFingerprints: string[];
  keyFingerprints: string[];
};

export type ParsedKeyring = {
  activeKey: PrivateKey;
  keys: PrivateKey[];
} & ({ kind: "legacy" } | { kind: "keyring"; metadata: KeyringMetadata });

function invalid(): never {
  throw new Error("The PGP keyring is invalid or unsupported. Restore a complete supported backup.");
}

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

async function readSingleKey(armor: string): Promise<PrivateKey> {
  // OpenPGP armor readers may stop at the first block. Reject trailing blocks
  // rather than silently discarding a second historical key.
  const trimmed = armor.trim();
  if (!trimmed.startsWith("-----BEGIN PGP PRIVATE KEY BLOCK-----") ||
      !trimmed.endsWith("-----END PGP PRIVATE KEY BLOCK-----") ||
      trimmed.split("-----BEGIN PGP ").length !== 2) invalid();
  const pgp = await import("openpgp");
  const keys = await pgp.readPrivateKeys({ armoredKeys: trimmed });
  const key = keys[0];
  if (keys.length !== 1 || !key) invalid();
  // isDecrypted() on a primary key means SOME packet is decrypted. A ring must
  // not silently accept a still-passphrase-protected historical subkey.
  if (!key.getKeys().every(({ keyPacket }) => "isDecrypted" in keyPacket && keyPacket.isDecrypted())) invalid();
  return key;
}

/** Validate the complete payload before exposing any member to a decrypt call. */
export async function parseKeyring(raw: string): Promise<ParsedKeyring> {
  try {
    if (!raw.trimStart().startsWith("{")) {
      // Preserve the existing armor reader: legacy exports may carry preambles,
      // large certification/UID sets or a GNU dummy primary with live subkeys.
      const pgp = await import("openpgp");
      const activeKey = await pgp.readPrivateKey({ armoredKey: raw });
      return { kind: "legacy", activeKey, keys: [activeKey] };
    }
    if (raw.length > MAX_KEYRING_BYTES || new TextEncoder().encode(raw).length > MAX_KEYRING_BYTES) invalid();
    const value: unknown = JSON.parse(raw);
    if (!record(value) || value.format !== "kypost-pgp-keyring-v1" ||
        typeof value.activeFingerprint !== "string" ||
        typeof value.materialGeneration !== "number" || !Number.isSafeInteger(value.materialGeneration) || value.materialGeneration < 1 ||
        !Array.isArray(value.keyFingerprints) || !value.keyFingerprints.every((item: unknown) => typeof item === "string") ||
        !Array.isArray(value.keys) || value.keys.length < 1 || value.keys.length > MAX_KEYS) invalid();

    const keys: PrivateKey[] = [];
    const primaryFingerprints = new Set<string>();
    for (const entry of value.keys) {
      if (!record(entry) || typeof entry.fingerprint !== "string" || typeof entry.privateKey !== "string" ||
          (entry.revocationCertificate !== undefined && typeof entry.revocationCertificate !== "string")) invalid();
      const key = await readSingleKey(entry.privateKey);
      const fingerprint = key.getFingerprint().toUpperCase();
      if (entry.fingerprint.toUpperCase() !== fingerprint || primaryFingerprints.has(fingerprint)) invalid();
      primaryFingerprints.add(fingerprint);
      keys.push(key);
    }
    const inventory = keys.flatMap(key => key.getKeys().map(member => member.getFingerprint().toUpperCase()));
    const declared = value.keyFingerprints.map((fingerprint: string) => fingerprint.toUpperCase());
    const activeFingerprint = value.activeFingerprint.toUpperCase();
    if (inventory.length > MAX_FINGERPRINTS || new Set(inventory).size !== inventory.length || new Set(declared).size !== declared.length ||
        inventory.length !== declared.length || !inventory.every(fingerprint => declared.includes(fingerprint))) invalid();
    const activeKey = keys.find(key => key.getFingerprint().toUpperCase() === activeFingerprint);
    if (!activeKey) invalid();
    const metadata: KeyringMetadata = { version: 1, materialGeneration: value.materialGeneration,
      primaryFingerprints: [...primaryFingerprints].sort(), keyFingerprints: inventory.slice().sort() };
    return { kind: "keyring", activeKey, keys, metadata };
  } catch {
    // Parser errors can contain hostile input; never surface key material.
    return invalid();
  }
}

/** IDs select candidates only; OpenPGP verifies the actual decryption. */
export function decryptionKeysFor(keyring: ParsedKeyring, message: Message<string>): PrivateKey[] {
  const ids = message.getEncryptionKeyIDs().map(id => id.toHex());
  // ponytail: bounded scan of at most 16 keys; index only if the ring cap grows.
  if (ids.some(id => /^0+$/.test(id))) return keyring.keys;
  const keys = keyring.keys.filter(key => key.getKeyIDs().some(id => ids.includes(id.toHex())));
  if (keys.length === 0) throw new Error("No retained PGP key matches this message.");
  return keys;
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

// Packet order can differ between Go and JS serialization. Compare the full
// multiset, including UID signatures/revocations, without rewriting private armor.
async function publicPackets(key: PublicKey): Promise<string[]> {
  const { PacketList } = await import("openpgp");
  return Array.from(key.toPacketList(), packet => {
    const single = new PacketList();
    single.push(packet);
    return Array.from(single.write(), byte => byte.toString(16).padStart(2, "0")).join("");
  }).sort();
}

/** Matches every member and current public packet; matching an active key alone is insufficient. */
export async function validateKeyringSnapshot(raw: string, snapshot: {
  fingerprint: string; publicKey: string; keyring: unknown;
}) {
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring") invalid();
  const metadata = parseKeyringMetadata(snapshot.keyring);
  if (JSON.stringify(metadata) !== JSON.stringify(ring.metadata) ||
      typeof snapshot.fingerprint !== "string" || ring.activeKey.getFingerprint().toUpperCase() !== snapshot.fingerprint.toUpperCase()) {
    throw new Error("This backup does not contain the current complete keyring. Older backups cannot recover newer keys; keep this copy for a future merge into an unlocked current ring.");
  }
  try {
    if (typeof snapshot.publicKey !== "string" || new TextEncoder().encode(snapshot.publicKey).length > MAX_KEYRING_BYTES) invalid();
    const pgp = await import("openpgp");
    const keys = await pgp.readKeys({ armoredKeys: snapshot.publicKey });
    const key = keys[0];
    if (keys.length !== 1 || !key || key.isPrivate() ||
        JSON.stringify(await publicPackets(key)) !== JSON.stringify(await publicPackets(ring.activeKey.toPublic()))) invalid();
  } catch {
    throw new Error("The backup's public key does not match the current keyring. Public identities or revocations may have changed; no key was restored.");
  }
  return ring;
}
