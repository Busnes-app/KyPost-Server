// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import fixture from "../../../testdata/pgp-keyring-v1.json";
import { parseKeyring } from "./pgpKeyring";
import { createRecoveryBackup, unwrapPrivateKey, wrapPrivateKey } from "./keyVault";

// The API module is mocked so these tests exercise the session logic — the
// state machine a cold-starting client depends on — without a server.
const getPGPBootstrap = vi.fn();
const rewrapPGPPrivateKey = vi.fn();
const rewrapPGPKeyring = vi.fn();
const putPGPKeyringRecovery = vi.fn();

vi.mock("../api/pgp", async (importOriginal) => ({
  ...await importOriginal<typeof import("../api/pgp")>(),
  getPasswordSnapshot: (...args: unknown[]) => getPGPBootstrap(...args),
  getPGPBootstrap: (...args: unknown[]) => getPGPBootstrap(...args),
  rewrapPGPPrivateKey: (...args: unknown[]) => rewrapPGPPrivateKey(...args),
  rewrapPGPKeyring: (...args: unknown[]) => rewrapPGPKeyring(...args),
  putPGPKeyringRecovery: (...args: unknown[]) => putPGPKeyringRecovery(...args)
}));

const SECRET = "-----BEGIN PGP PRIVATE KEY BLOCK-----\nkey\n-----END PGP PRIVATE KEY BLOCK-----";
const OLD_PASSWORD = "old-account-password";
const NEW_PASSWORD = "new-account-password";
const TIMEOUT = 30_000;

function bootstrapFixture(overrides: Record<string, unknown> = {}) {
  return {
    pgpRevision: 7,
    hasIdentity: true,
    protection: "client",
    fingerprint: "FPR",
    keyId: "KID",
    publicKey: "pub",
    keySource: "generated",
    createdAt: "2026-07-25T00:00:00Z",
    wrappedPrivateKey: "",
    unlockRequired: true,
    canDecryptServerSide: false,
    migrationAvailable: false,
    signerKeys: [],
    suggestedUserIDs: ["me@example.com"],
    displayName: "me",
    payloadEndpoint: "/api/mail/pgp-payload",
    ...overrides
  };
}

let session: typeof import("./pgpSession");

beforeEach(async () => {
  vi.resetModules();
  getPGPBootstrap.mockReset();
  rewrapPGPPrivateKey.mockReset();
  rewrapPGPKeyring.mockReset();
  putPGPKeyringRecovery.mockReset();
  session = await import("./pgpSession");
});

afterEach(() => {
  session.clearPGPSession();
});

describe("cold start", () => {
  it("reports a client-protected account as needing an unlock", async () => {
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: "{}" }));
    await session.loadPGPSession();
    expect(session.isClientProtected()).toBe(true);
    expect(session.needsUnlock()).toBe(true);
  });

  it("does not treat a legacy account as needing an unlock", async () => {
    getPGPBootstrap.mockResolvedValue(
      bootstrapFixture({ protection: "server", unlockRequired: false, migrationAvailable: true })
    );
    await session.loadPGPSession();
    expect(session.isClientProtected()).toBe(false);
    expect(session.needsUnlock()).toBe(false);
  });

  // A failed bootstrap must not look like "this account has no PGP key" —
  // that is how a client offers to generate a second identity over an
  // existing one.
  it("surfaces a fetch failure instead of reporting no identity", async () => {
    getPGPBootstrap.mockRejectedValue(new Error("network down"));
    const state = await session.loadPGPSession();
    expect(state.error).toContain("network down");
    expect(state.bootstrap).toBeNull();
    expect(session.isClientProtected()).toBe(false);
  });

  it(
    "unlocks with the right password and refuses the wrong one",
    async () => {
      const envelope = await wrapPrivateKey(SECRET, OLD_PASSWORD);
      getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: JSON.stringify(envelope) }));
      await session.loadPGPSession();

      await expect(session.unlockPGPSession("wrong-password-here")).rejects.toBeTruthy();
      expect(session.needsUnlock()).toBe(true);

      await session.unlockPGPSession(OLD_PASSWORD);
      expect(session.needsUnlock()).toBe(false);

      session.lockPGPSession();
      expect(session.needsUnlock()).toBe(true);
    },
    TIMEOUT
  );
});

describe("password change rewrap", () => {
  it(
    "returns an envelope that opens with the new password and not the old",
    async () => {
      const envelope = await wrapPrivateKey(SECRET, OLD_PASSWORD);
      getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: JSON.stringify(envelope) }));
      await session.loadPGPSession();

      const rewrapped = await session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD);
      expect(rewrapped).not.toBeNull();

      // Nothing is uploaded here at all any more. The envelope is returned as
      // DATA so the caller can send it in the same request as the credential —
      // the two used to be separate requests, and a dropped connection between
      // them stranded the key permanently.
      expect(rewrapPGPPrivateKey).not.toHaveBeenCalled();

      const parsed = JSON.parse(rewrapped.rewrappedPgpKey!);
      const { unwrapPrivateKey } = await import("./keyVault");
      await expect(unwrapPrivateKey(parsed, NEW_PASSWORD)).resolves.toBe(SECRET);
      await expect(unwrapPrivateKey(parsed, OLD_PASSWORD)).rejects.toBeTruthy();
    },
    TIMEOUT
  );

  // Rewrapping with the wrong current password must fail BEFORE anything is
  // sent, so the caller aborts with nothing half-applied.
  it(
    "fails on a wrong current password without uploading anything",
    async () => {
      const envelope = await wrapPrivateKey(SECRET, OLD_PASSWORD);
      getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: JSON.stringify(envelope) }));
      await session.loadPGPSession();

      await expect(session.rewrappedEnvelopeFor("not-the-old-password", NEW_PASSWORD)).rejects.toBeTruthy();
      expect(rewrapPGPPrivateKey).not.toHaveBeenCalled();
    },
    TIMEOUT
  );

  it("is a no-op for an account with no client-protected key", async () => {
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ protection: "server", wrappedPrivateKey: "" }));
    await session.loadPGPSession();
    await expect(session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD)).resolves.toEqual({ expectedRevision: 7 });
    expect(rewrapPGPPrivateKey).not.toHaveBeenCalled();
  });
});

// The recovery path for an envelope that is out of step with the account
// password. Before this existed there was none: every rewrap derived from the
// CURRENT password, and a stale envelope by definition does not open with it, so
// the only escape was deleting the identity and losing every message ever
// encrypted to it.
describe("stale-envelope recovery", () => {
  it(
    "re-seals the already-unlocked key under the current password",
    async () => {
      // The stored envelope is sealed under an OLDER password than the account's.
      const stale = await wrapPrivateKey(SECRET, OLD_PASSWORD);
      getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: JSON.stringify(stale) }));
      await session.loadPGPSession();

      // The user unlocks with the older password, as the UI instructs.
      await session.unlockPGPSession(OLD_PASSWORD);

      rewrapPGPPrivateKey.mockResolvedValue({ ok: true });
      await session.rewrapUnlockedKeyUnder(NEW_PASSWORD);

      expect(rewrapPGPPrivateKey).toHaveBeenCalledTimes(1);
      const uploaded = JSON.parse(rewrapPGPPrivateKey.mock.calls[0][0] as string);
      const { unwrapPrivateKey } = await import("./keyVault");
      await expect(unwrapPrivateKey(uploaded, NEW_PASSWORD)).resolves.toBe(SECRET);
      // The CURRENT account password also goes up as the step-up credential:
      // the server refuses to overwrite an envelope it cannot inspect on a
      // session alone. Sending the old one here would fail the confirmation and
      // leave the stale envelope in place — the exact state this recovers from.
      expect(rewrapPGPPrivateKey.mock.calls[0][1]).toBe(NEW_PASSWORD);
    },
    TIMEOUT
  );

  it("refuses when the vault is locked, rather than uploading garbage", async () => {
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: "" }));
    await session.loadPGPSession();
    session.lockPGPSession();
    await expect(session.rewrapUnlockedKeyUnder(NEW_PASSWORD)).rejects.toBeTruthy();
    expect(rewrapPGPPrivateKey).not.toHaveBeenCalled();
  });
});

describe("logout", () => {
  it(
    "drops the unwrapped key so the next person at this browser cannot read mail",
    async () => {
      const envelope = await wrapPrivateKey(SECRET, OLD_PASSWORD);
      getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: JSON.stringify(envelope) }));
      await session.loadPGPSession();
      await session.unlockPGPSession(OLD_PASSWORD);

      const { isUnlocked } = await import("./keyVault");
      expect(isUnlocked()).toBe(true);

      session.clearPGPSession();
      expect(isUnlocked()).toBe(false);
      expect(session.pgpSessionState().bootstrap).toBeNull();
    },
    TIMEOUT
  );
});


it("refuses keyring rewrap through the legacy password-change API", async () => {
  const envelope = await wrapPrivateKey(JSON.stringify({ format: "kypost-pgp-keyring-v1" }), OLD_PASSWORD);
  getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: JSON.stringify(envelope) }));
  await session.loadPGPSession();
  await expect(session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD)).rejects.toThrow(/lifecycle upgrade/);
  await session.unlockPGPSession(OLD_PASSWORD);
  await expect(session.rewrapUnlockedKeyUnder(NEW_PASSWORD)).rejects.toThrow(/lifecycle upgrade/);
  expect(rewrapPGPPrivateKey).not.toHaveBeenCalled();
}, TIMEOUT);


describe("revision-bound preparation", () => {
  it("does not rebind an unlocked key when a newer same-key bootstrap arrives", async () => {
    const wrappedPrivateKey = JSON.stringify(await wrapPrivateKey(SECRET, OLD_PASSWORD));
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey, pgpRevision: 7 }));
    await session.loadPGPSession();
    await session.unlockPGPSession(OLD_PASSWORD);
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey, pgpRevision: 8 }));
    await session.loadPGPSession();
    expect(session.unlockedPGPIdentity()).toEqual({ fingerprint: "FPR", pgpRevision: 7 });
    rewrapPGPPrivateKey.mockRejectedValue(new Error("PGP state changed; reload"));
    await expect(session.rewrapUnlockedKeyUnder(NEW_PASSWORD)).rejects.toThrow(/state changed/);
    expect(rewrapPGPPrivateKey).toHaveBeenCalledExactlyOnceWith(expect.any(String), NEW_PASSWORD, "FPR", 7);
    session.lockPGPSession();
    expect(() => session.unlockedPGPIdentity()).toThrow();
  }, TIMEOUT);

  it("returns the revision belonging to the rewrapped bytes despite a concurrent refresh", async () => {
    const wrappedPrivateKey = JSON.stringify(await wrapPrivateKey(SECRET, OLD_PASSWORD));
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey, pgpRevision: 0 }));
    const preparing = session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD);
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: "bad", pgpRevision: 9 }));
    await session.loadPGPSession();
    const result = await preparing;
    expect(result.expectedRevision).toBe(0);
    const { unwrapPrivateKey } = await import("./keyVault");
    expect(await unwrapPrivateKey(JSON.parse(result.rewrappedPgpKey!), NEW_PASSWORD)).toBe(SECRET);
  }, TIMEOUT);

  it.each([undefined, -1, 1.5, Number.MAX_SAFE_INTEGER + 1])("refuses missing/invalid revision %s", async (pgpRevision) => {
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ pgpRevision, protection: "" }));
    await expect(session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD)).rejects.toThrow(/revision unavailable/);
  });

  it("refuses a corrupt normal envelope but preserves it on a forced reset", async () => {
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: "bad" }));
    await expect(session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD)).rejects.toThrow(/cannot be read/);
    getPGPBootstrap.mockResolvedValue(bootstrapFixture({ wrappedPrivateKey: "", mustChangePassword: true }));
    await expect(session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD)).resolves.toEqual({ expectedRevision: 7 });
  });
});

it("rewraps exact complete ring bytes against a matching current snapshot", async () => {
  const raw = JSON.stringify(fixture.ring);
  const ring = await parseKeyring(raw);
  if (ring.kind !== "keyring") throw new Error("expected ring");
  const snapshot = bootstrapFixture({ fingerprint: ring.activeKey.getFingerprint(), publicKey: ring.activeKey.toPublic().armor(),
    keyring: ring.metadata, wrappedPrivateKey: JSON.stringify(await wrapPrivateKey(raw, OLD_PASSWORD)) });
  getPGPBootstrap.mockResolvedValue(snapshot);
  const prepared = await session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD);
  expect(prepared.keyringVersion).toBe(1);
  expect(prepared.expectedRevision).toBe(7);
  expect(await unwrapPrivateKey(JSON.parse(prepared.rewrappedPgpKey ?? ""), NEW_PASSWORD)).toBe(raw);
  expect(session.pgpSessionState().unlocked).toBe(false);
  getPGPBootstrap.mockResolvedValueOnce(snapshot).mockResolvedValueOnce({ ...snapshot, pgpRevision: 8 });
  await expect(session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD)).rejects.toThrow(/state changed/);
  getPGPBootstrap.mockResolvedValue({ ...snapshot, keyring: { ...ring.metadata, materialGeneration: 99 } });
  await expect(session.rewrappedEnvelopeFor(OLD_PASSWORD, NEW_PASSWORD)).rejects.toThrow(/complete keyring/);
}, TIMEOUT);

describe("complete-ring restore confirmation", () => {
  for (const outcome of ["success", "lost response", "different ciphertext", "newer revision", "failed read", "session changed"]) {
    it(outcome, async () => {
      const raw = JSON.stringify(fixture.ring);
      const ring = await parseKeyring(raw);
      if (ring.kind !== "keyring") throw new Error("expected ring");
      getPGPBootstrap.mockResolvedValue(bootstrapFixture({ fingerprint: ring.activeKey.getFingerprint(),
        publicKey: ring.activeKey.toPublic().armor(), keyring: ring.metadata }));
      const snapshot = (await session.loadPGPSession()).bootstrap;
      if (!snapshot) throw new Error("missing snapshot");
      rewrapPGPKeyring.mockImplementation(async (input: { wrapped: string; expectedRevision: number }) => {
        expect(input.expectedRevision).toBe(7);
        expect(await unwrapPrivateKey(JSON.parse(input.wrapped), NEW_PASSWORD)).toBe(raw);
        getPGPBootstrap.mockResolvedValue({ ...snapshot, pgpRevision: outcome === "newer revision" ? 9 : 8,
          wrappedPrivateKey: outcome === "different ciphertext" ? "different" : input.wrapped });
        if (outcome === "failed read") getPGPBootstrap.mockRejectedValue(new Error("network"));
        if (outcome === "session changed") session.clearPGPSession();
        if (outcome === "lost response") throw new Error("connection lost after commit");
      });
      const restoring = session.restorePGPKeyring(raw,NEW_PASSWORD,snapshot,() => true);
      if (outcome === "success" || outcome === "lost response") await expect(restoring).resolves.toBe(8);
      else await expect(restoring).rejects.toThrow(/could not be confirmed/);
      expect(rewrapPGPKeyring).toHaveBeenCalledTimes(1);
      expect(session.pgpSessionState().unlocked).toBe(outcome === "success" || outcome === "lost response");
    },TIMEOUT);
  }
});

describe("complete-ring recovery slot confirmation", () => {
  for (const outcome of ["success", "different ciphertext", "newer revision", "wrong metadata", "stale preparation", "session changed"]) {
    it(outcome,async () => {
      const raw = JSON.stringify(fixture.ring);
      const ring = await parseKeyring(raw);
      if (ring.kind !== "keyring") throw new Error("expected ring");
      const snapshot = bootstrapFixture({ fingerprint:ring.activeKey.getFingerprint(),publicKey:ring.activeKey.toPublic().armor(),keyring:ring.metadata });
      const created = await createRecoveryBackup(raw,{ fingerprint:snapshot.fingerprint,publicKey:snapshot.publicKey,keyring:ring.metadata });
      getPGPBootstrap.mockResolvedValue({...snapshot,pgpRevision:outcome === "stale preparation" ? 8 : 7});
      putPGPKeyringRecovery.mockImplementation(async (input:{envelope:string;expectedRevision:number;isCurrent:()=>boolean}) => {
        expect(input.expectedRevision).toBe(7);
        expect(input.envelope).toBe(JSON.stringify(created.backup.envelope));
        if (outcome === "session changed") session.clearPGPSession();
        return {...snapshot,envelope:outcome === "different ciphertext" ? "other" : input.envelope,
          pgpRevision:outcome === "newer revision" ? 9 : 8,
          keyring:outcome === "wrong metadata" ? {...ring.metadata,materialGeneration:99} : ring.metadata};
      });
      const storing = session.storePGPKeyringRecovery(created.backup,created.secret,NEW_PASSWORD,7,()=>true);
      if (outcome === "success") await expect(storing).resolves.toBeUndefined();
      else await expect(storing).rejects.toThrow(outcome === "stale preparation" ? /state changed/ : /could not be confirmed/);
      expect(putPGPKeyringRecovery).toHaveBeenCalledTimes(outcome === "stale preparation" ? 0 : 1);
      expect(session.pgpSessionState().unlocked).toBe(false);
    },TIMEOUT);
  }
});
