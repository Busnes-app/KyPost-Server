// Renders the fingerprint/expiry of a contact's armored PGP key, or the
// parse error. The OpenPGP engine loads lazily, only once a contact has a key.

import { useEffect, useState } from "react";
import { pgpEngine } from "../../lib/pgpEngine";

export type PGPKeyResult = { fingerprint: string; keyId: string; expires?: string } | { error: string };

export async function validatePGPKey(armored: string): Promise<PGPKeyResult | null> {
  const trimmed = armored.trim();
  if (!trimmed) return null;
  try {
    const key = await (await pgpEngine()).inspect(trimmed);
    const expires = key.expires > 0 ? new Date(key.expires * 1000).toISOString().slice(0, 10) : undefined;
    return { fingerprint: key.fingerprint.toLowerCase(), keyId: key.keyIDs[0].toLowerCase(), expires };
  } catch (error) {
    return { error: error instanceof Error ? error.message : "invalid key" };
  }
}

export function PGPKeyInfo({ armoredKey }: { armoredKey: string }) {
  const [info, setInfo] = useState<PGPKeyResult | null>(null);

  useEffect(() => {
    let cancelled = false;
    void validatePGPKey(armoredKey).then((result) => {
      if (!cancelled) setInfo(result);
    });
    return () => {
      cancelled = true;
    };
  }, [armoredKey]);

  if (!info) return null;
  if ("error" in info) {
    return <p className="contacts-pgp-error">Could not parse key: {info.error}</p>;
  }
  return (
    <p className="contacts-pgp-fingerprint">
      Fingerprint: {info.fingerprint}
      {info.expires ? ` · Expires ${info.expires}` : ""}
    </p>
  );
}
