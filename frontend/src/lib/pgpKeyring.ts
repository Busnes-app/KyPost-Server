// Reader for sealed vault plaintext. Account conversion and keyring writes are
// deliberately unavailable until the server/native lifecycle gates are ready.
import type { Message, PrivateKey } from "openpgp";

const MAX_KEYRING_BYTES = 128 << 10;
const MAX_KEYS = 16;

export type ParsedKeyring = {
  activeKey: PrivateKey;
  keys: PrivateKey[];
};

function invalid(): never {
  throw new Error("The PGP keyring is invalid or unsupported. Restore a complete supported backup.");
}

function record(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

async function readSingleKey(armor: string, requireComplete: boolean): Promise<PrivateKey> {
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
  // not silently accept a still-passphrase-protected historical subkey. Legacy
  // armor retains OpenPGP's existing policy, including GNU dummy primary keys.
  if (requireComplete && !key.getKeys().every(({ keyPacket }) => "isDecrypted" in keyPacket && keyPacket.isDecrypted())) invalid();
  return key;
}

/** Validate the complete payload before exposing any member to a decrypt call. */
export async function parseKeyring(raw: string): Promise<ParsedKeyring> {
  if (raw.length > MAX_KEYRING_BYTES || new TextEncoder().encode(raw).length > MAX_KEYRING_BYTES) invalid();
  try {
    if (raw.trimStart().startsWith("-----BEGIN PGP PRIVATE KEY BLOCK-----")) {
      const activeKey = await readSingleKey(raw, false);
      return { activeKey, keys: [activeKey] };
    }
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
      const key = await readSingleKey(entry.privateKey, true);
      const fingerprint = key.getFingerprint().toUpperCase();
      if (entry.fingerprint !== fingerprint || primaryFingerprints.has(fingerprint)) invalid();
      primaryFingerprints.add(fingerprint);
      keys.push(key);
    }
    const inventory = keys.flatMap(key => key.getKeys().map(member => member.getFingerprint().toUpperCase()));
    const declared = value.keyFingerprints;
    if (new Set(inventory).size !== inventory.length || new Set(declared).size !== declared.length ||
        inventory.length !== declared.length || !inventory.every(fingerprint => declared.includes(fingerprint))) invalid();
    const activeKey = keys.find(key => key.getFingerprint().toUpperCase() === value.activeFingerprint);
    if (!activeKey) invalid();
    return { activeKey, keys };
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
