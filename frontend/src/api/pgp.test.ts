import { beforeEach, describe, expect, it, vi } from "vitest";
import { deleteDeviceEnvelope, putDeviceEnvelope, putRecoveryEnvelope, getRecoveryBackup, getPasswordSnapshot, rewrapPGPKeyring, putPGPKeyringRecovery } from "./pgp";
import type { DeviceEnvelope } from "../lib/deviceEnrollment";

const getJSON = vi.fn();
const putJSON = vi.fn();
const postJSON = vi.fn();
const deleteJSON = vi.fn();

vi.mock("./client", () => ({
  getJSON: (path: string) => getJSON(path),
  postJSON: (path: string, body: unknown) => postJSON(path,body),
  putJSON: (path: string, body: unknown) => putJSON(path, body),
  deleteJSON: (path: string, body: unknown) => deleteJSON(path, body)
}));

// stepUp derives a credential the server can verify; the shape is auth.ts's
// business, so this pins only that the step-up IS attached.
const deriveCredential = vi.fn(async () => ({ kind: "test" }));
vi.mock("./auth", () => ({
  deriveCredential: () => deriveCredential(),
  credentialFields: () => ({ password: "hunter2" })
}));

const ENVELOPE: DeviceEnvelope = {
  v: 2,
  alg: "ECDH-P256+HKDF-SHA256+A256GCM",
  epk: "EPK",
  iv: "IV",
  ct: "CT"
};

beforeEach(() => {
  getJSON.mockReset();
  putJSON.mockReset();
  deleteJSON.mockReset();
  putJSON.mockResolvedValue({ ok: true });
  deleteJSON.mockResolvedValue({ ok: true });
});

describe("putDeviceEnvelope", () => {
  it("writes the device slot with the id escaped and the prefix literal", async () => {
    await putDeviceEnvelope("dev:1", ENVELOPE, "hunter2", 0, "DEVKEY");

    expect(putJSON).toHaveBeenCalledWith("/api/pgp/identity/envelope/device:dev%3A1", {
      envelope: JSON.stringify(ENVELOPE),
      enrollmentPublicKey: "DEVKEY",
      password: "hunter2", expectedRevision: 0
    });
  });

  it("names the material generation it sealed on a converted account", async () => {
    await putDeviceEnvelope("dev:1", ENVELOPE, "hunter2", 4, "DEVKEY", 2);

    expect(putJSON).toHaveBeenCalledWith("/api/pgp/identity/envelope/device:dev%3A1", {
      envelope: JSON.stringify(ENVELOPE),
      enrollmentPublicKey: "DEVKEY", materialGeneration: 2,
      password: "hunter2", expectedRevision: 4
    });
  });
});

describe("deleteDeviceEnvelope", () => {
  // The route decodes the body unconditionally and 400s a bodyless request
  // rather than treating it as "no credential needed" — so the credential must
  // travel in the body, not as a query parameter.
  it("sends the step-up in the body", async () => {
    await deleteDeviceEnvelope("dev:1", "hunter2", 0);

    expect(deleteJSON).toHaveBeenCalledWith("/api/pgp/identity/envelope/device:dev%3A1", {
      password: "hunter2", expectedRevision: 0
    });
  });
});


describe("recovery envelope", () => {
  const envelope = { v: 2, kdf: "PBKDF2-SHA256", iterations: 600000, salt: "AA==", iv: "AA==", ciphertext: "AA==" } as const;
  it("includes both account step-up and the expected identity on upload", async () => {
    await putRecoveryEnvelope(envelope, "hunter2", "FPR", 0);
    expect(putJSON).toHaveBeenCalledWith("/api/pgp/identity/envelope/recovery", {
      envelope: JSON.stringify(envelope), expectedFingerprint: "FPR", password: "hunter2", expectedRevision: 0
    });
  });
  it("rebuilds a recovery file from one server snapshot", async () => {
    getJSON.mockResolvedValue({ envelope: JSON.stringify(envelope), fingerprint: "FPR", publicKey: "PUBLIC" });
    expect(await getRecoveryBackup()).toEqual({ format: "kypost-pgp-recovery-v1", envelope, fingerprint: "FPR", publicKey: "PUBLIC" });
    expect(getJSON).toHaveBeenCalledWith("/api/pgp/identity/envelope/recovery");
  });
  it.each([
    { envelope: "not-json", fingerprint: "FPR", publicKey: "PUBLIC" },
    { envelope: JSON.stringify(envelope) }
  ])("refuses an unreadable copy or missing identity snapshot", async (value) => {
    getJSON.mockResolvedValue(value);
    await expect(getRecoveryBackup()).rejects.toThrow(/cannot be read/);
  });
});


describe("password snapshot boundary", () => {
  it("accepts a forced-reset snapshot with revision zero and no key bytes", async () => {
    const snapshot = { pgpRevision: 0, protection: "client", wrappedPrivateKey: "", mustChangePassword: true };
    getJSON.mockResolvedValue(snapshot);
    expect(await getPasswordSnapshot()).toEqual(snapshot);
    expect(getJSON).toHaveBeenCalledWith("/api/auth/password");
  });
  it.each([
    {},
    { pgpRevision: 0, wrappedPrivateKey: "", mustChangePassword: false },
    { pgpRevision: 0, protection: "client", mustChangePassword: false },
    { pgpRevision: 0, protection: "other", wrappedPrivateKey: "", mustChangePassword: false },
    { pgpRevision: -1, protection: "", wrappedPrivateKey: "", mustChangePassword: false }
  ])("refuses incomplete preparation data instead of assuming a keyless account", async (snapshot) => {
    getJSON.mockResolvedValue(snapshot);
    await expect(getPasswordSnapshot()).rejects.toThrow();
  });
});


describe("versioned server recovery snapshots", () => {
  const fingerprint = "A".repeat(40);
  const keyring = { version: 1, materialGeneration: 3, primaryFingerprints: [fingerprint], keyFingerprints: [fingerprint] };
  const envelope = { v: 2, kdf: "PBKDF2-SHA256", iterations: 600000, salt: "AA==", iv: "AA==", ciphertext: "AA==" };
  it("constructs a v2 copy from a converted snapshot without downgrading metadata", async () => {
    getJSON.mockResolvedValue({ fingerprint, publicKey: "PUBLIC", keyring, envelope: JSON.stringify(envelope) });
    expect(await getRecoveryBackup()).toEqual({ format: "kypost-pgp-recovery-v2", fingerprint, publicKey: "PUBLIC", keyring, envelope });
  });
  it("refuses unknown metadata instead of returning a legacy backup", async () => {
    getJSON.mockResolvedValue({ fingerprint, publicKey: "PUBLIC", keyring: { ...keyring, version: 2 }, envelope: JSON.stringify(envelope) });
    await expect(getRecoveryBackup()).rejects.toThrow();
  });
});

it("opts into complete-ring rewrap with step-up and the original revision", async () => {
  await rewrapPGPKeyring({ wrapped: "SEALED", password: "password", expectedFingerprint: "FPR", expectedRevision: 7, isCurrent: () => true });
  expect(postJSON).toHaveBeenCalledWith("/api/pgp/identity/rewrap", {
    wrapped: "SEALED", password: "hunter2", expectedFingerprint: "FPR", expectedRevision: 7, keyringVersion: 1
  });
});

it("does not POST if the restore session changes during credential derivation", async () => {
  postJSON.mockClear();
  let current = true;
  deriveCredential.mockImplementationOnce(async () => { current = false; return { kind: "test" }; });
  await expect(rewrapPGPKeyring({ wrapped: "SEALED", password: "password", expectedFingerprint: "FPR",
    expectedRevision: 7, isCurrent: () => current })).rejects.toThrow(/session changed/);
  expect(postJSON).not.toHaveBeenCalled();
});

it("confirms the slot after a lost PUT response without retrying",async () => {
  putJSON.mockReset();getJSON.mockReset();
  putJSON.mockRejectedValueOnce(new Error("lost response"));
  getJSON.mockResolvedValueOnce({envelope:"SEALED",pgpRevision:8});
  await expect(putPGPKeyringRecovery({envelope:"SEALED",password:"password",expectedFingerprint:"FPR",expectedRevision:7,isCurrent:()=>true})).resolves.toEqual({envelope:"SEALED",pgpRevision:8});
  expect(putJSON).toHaveBeenCalledTimes(1);
  expect(putJSON).toHaveBeenCalledWith("/api/pgp/identity/envelope/recovery",{envelope:"SEALED",password:"hunter2",expectedFingerprint:"FPR",expectedRevision:7,keyringVersion:1});
});
it("does not PUT after session change during step-up",async () => {
  putJSON.mockClear();let current=true;
  deriveCredential.mockImplementationOnce(async()=>{current=false;return {kind:"test"};});
  await expect(putPGPKeyringRecovery({envelope:"SEALED",password:"password",expectedFingerprint:"FPR",expectedRevision:7,isCurrent:()=>current})).rejects.toThrow(/session changed/);
  expect(putJSON).not.toHaveBeenCalled();
});
