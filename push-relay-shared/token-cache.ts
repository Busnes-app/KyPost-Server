/**
 * Provider credentials (Google access tokens, APNs provider JWTs) cached in KV,
 * with this isolate's memory behind it. KV writes have a daily account quota; a
 * write KV refuses must not fail the send that minted a valid token, nor make
 * every later send mint again (Apple refuses provider tokens renewed more often
 * than every 20 minutes).
 */

const memory = new Map<string, { token: string; until: number }>();

/** KV's token, else this isolate's, else `mint()`'s, which is then saved to both. */
export async function cachedToken(
  cache: KVNamespace,
  key: string,
  fresh: boolean,
  mint: () => Promise<{ token: string; ttlSeconds: number }>,
): Promise<string> {
  // `fresh` skips both reads: a KV delete is not guaranteed visible to the next get.
  if (!fresh) {
    const stored = await cache.get(key);
    if (stored) {
      return stored;
    }
    const held = memory.get(key);
    if (held && held.until > Date.now()) {
      return held.token;
    }
  }
  const { token, ttlSeconds } = await mint();
  memory.set(key, { token, until: Date.now() + ttlSeconds * 1000 });
  await cache.put(key, token, { expirationTtl: ttlSeconds }).catch(() => {});
  return token;
}

/** Forgets a token the provider refused. */
export async function dropToken(cache: KVNamespace, key: string): Promise<void> {
  memory.delete(key);
  await cache.delete(key).catch(() => {});
}
