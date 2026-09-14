import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { AuthContext } from "../auth";
import type { KeyringMetadata } from "../lib/pgpKeyring";
import { SecurityPage } from "./SecurityPage";

const getJSON = vi.fn();
const postJSON = vi.fn();
const putJSON = vi.fn();
const deleteJSON = vi.fn();

vi.mock("../api/client", () => ({
  getJSON: (url: string) => getJSON(url),
  postJSON: (url: string, body: unknown) => postJSON(url, body),
  putJSON: (url: string, body: unknown) => putJSON(url, body),
  deleteJSON: (url: string, body?: unknown) => deleteJSON(url, body),
  toErrorMessage: (e: unknown, fallback: string) => (e instanceof Error ? e.message : fallback)
}));

const SESSION = {
  bootstrap: {
    pgpRevision: 7,
    keyring: undefined as KeyringMetadata | undefined,
    protection: "client" as const,
    envelopeSlots: ["password"],
    fingerprint: "ABCDEF0123456789",
    publicKey: "PUB",
    suggestedUserIDs: ["gwen@example.com"],
    displayName: "Gwen",
    migrationAvailable: false
  },
  unlocked: true
};

vi.mock("../lib/pgpSession", () => ({
  subscribePGPSession: (fn: (s: unknown) => void) => {
    fn(SESSION);
    return () => {};
  },
  loadPGPSession: async () => SESSION,
  unlockedPGPIdentity: () => ({ fingerprint: BACKUP.fingerprint, pgpRevision: 7 }),
  acceptCommittedPGPKey: (armored: string) => unlockWithArmoredKey(armored),
  lockPGPSession: () => {},
  unlockPGPSession: async () => {},
  rewrapUnlockedKeyUnder: async () => {}
}));

const BACKUP = {
  format: "kypost-pgp-recovery-v1",
  fingerprint: "ABCDEF0123456789",
  publicKey: "PUB",
  envelope: { v: 2, kdf: "PBKDF2-SHA256", iterations: 600000, salt: "salt", iv: "iv", ciphertext: "ct" }
};
// OpenPGP packet tests run in node (keyringRecovery.test.ts); jsdom has a
// separate Uint8Array realm. Here test the UI's snapshot/gating side effects.
const validateKeyringSnapshot = vi.fn();
vi.mock("../lib/pgpKeyring", async importOriginal => ({
  ...await importOriginal<typeof import("../lib/pgpKeyring")>(),
  validateKeyringSnapshot: (...args: unknown[]) => validateKeyringSnapshot(...args)
}));
const importIdentity = vi.fn();
const restoreRecoveryBackup = vi.fn();
const unlockWithArmoredKey = vi.fn();
vi.mock("../api/auth", () => ({
  deriveCredential: async () => ({ authSecret: "derived-account-credential" }),
  credentialFields: () => ({ authSecret: "derived-account-credential" })
}));

const createRecoveryBackup = vi.fn(async () => ({
  backup: BACKUP,
  secret: "SECRET-ABCD-1234"
}));

vi.mock("../lib/keyVault", async (importOriginal) => ({
  ...await importOriginal<typeof import("../lib/keyVault")>(),
  createRecoveryBackup: (...a: unknown[]) => createRecoveryBackup(...(a as [])),
  requireUnlockedKey: () => "ARMORED",
  requireUnlockedKeyMaterial: () => "RING",
  restoreRecoveryBackup: (...args: unknown[]) => restoreRecoveryBackup(...args),
  wrapPrivateKey: async () => ({}),
  unlockWithArmoredKey: (...args: unknown[]) => unlockWithArmoredKey(...args)
}));

vi.mock("../lib/pgpClient", () => ({
  generateIdentity: async () => ({}),
  importIdentity: (...args: unknown[]) => importIdentity(...args)
}));

afterEach(cleanup);

beforeEach(() => {
  vi.clearAllMocks();
  localStorage.clear();
  SESSION.unlocked = true;
  SESSION.bootstrap.keyring = undefined;
  validateKeyringSnapshot.mockReset();
  validateKeyringSnapshot.mockResolvedValue({ kind: "keyring" });
  SESSION.bootstrap.pgpRevision = 7;
  SESSION.bootstrap.envelopeSlots = ["password"];
  importIdentity.mockResolvedValue({ fingerprint: BACKUP.fingerprint, armoredPrivateKey: "ARMORED", armoredPublicKey: "PUB" });
  restoreRecoveryBackup.mockResolvedValue({ ...BACKUP, privateKey: "ARMORED" });
  putJSON.mockResolvedValue({ ok: true });
  postJSON.mockResolvedValue({ ok: true, pgpRevision: 8 });
  vi.spyOn(window, "prompt").mockReturnValue("account-password");
  // jsdom has no object URL implementation; saveRecoveryBackup needs one.
  (URL as unknown as { createObjectURL: unknown }).createObjectURL = () => "blob:x";
  (URL as unknown as { revokeObjectURL: unknown }).revokeObjectURL = () => {};

  getJSON.mockImplementation((url: string) => {
    if (url === "/api/pgp/bootstrap") return Promise.resolve(SESSION.bootstrap);
    if (url === "/api/pgp/identity/envelope/recovery") return Promise.resolve({ envelope: JSON.stringify(BACKUP.envelope), fingerprint: BACKUP.fingerprint, publicKey: BACKUP.publicKey, keyring: SESSION.bootstrap.keyring });
    if (url === "/api/mfa/status") {
      return Promise.resolve({
        totpEnabled: true,
        recoveryCodesRemaining: 8,
        pushMfaEnabled: false,
        approverDevices: []
      });
    }
    if (url === "/api/pgp/identity") {
      return Promise.resolve({
        pgpRevision: 7,
        fingerprint: "ABCDEF0123456789",
        keyId: "0123456789",
        publicKey: "PUB",
        source: "generated",
        createdAt: "2026-01-01T00:00:00Z"
      });
    }
    if (url === "/api/pgp/discovery") {
      return Promise.resolve({
        autoEncryptWhenKeyKnown: true,
        storeDiscoveredKeys: true,
        advertiseAutocrypt: true,
        publishWKD: true
      });
    }
    if (url.startsWith("/api/pgp/discovery/suppressions")) {
      return Promise.resolve({ suppressions: [] });
    }
    if (url.startsWith("/api/notifications/native/devices")) {
      return Promise.resolve({ devices: [], deliveryMode: "push" });
    }
    if (url.startsWith("/api/contacts")) {
      return Promise.resolve([]);
    }
    return Promise.resolve({});
  });
});

function renderPage(tab = "mail") {
  return render(
    <MemoryRouter initialEntries={[`/security?tab=${tab}`]}>
      <AuthContext.Provider value={{ authenticated: true, userId: "user-1" }}><SecurityPage /></AuthContext.Provider>
    </MemoryRouter>
  );
}

describe("recoverySecret survives a tab switch", () => {
  it("still shows the one-time secret after leaving and returning to Encryption", async () => {
    const user = userEvent.setup();
    renderPage("mail");

    await screen.findByRole("button", { name: "Download recovery backup" });
    await user.click(screen.getByRole("button", { name: "Download recovery backup" }));

    await screen.findByText("SECRET-ABCD-1234");

    await user.click(screen.getByRole("tab", { name: "Devices" }));
    expect(screen.queryByText("SECRET-ABCD-1234")).toBeNull();

    await user.click(screen.getByRole("tab", { name: "Encryption" }));
    await screen.findByText("SECRET-ABCD-1234");

    // Only one backup was ever created — the secret came back from state,
    // not from a second createRecoveryBackup() call.
    expect(createRecoveryBackup).toHaveBeenCalledTimes(1);
  });
});

describe("recovery backup with a drifted identity response", () => {
  // POST /api/pgp/identity/client used to answer with users.Public, whose PGP
  // fields are named pgpFingerprint — so after generating or migrating to a
  // client-held key, the identity this page held had no `fingerprint` at all.
  // "Download recovery backup" then died on `fingerprint.slice(0, 8)`, AFTER
  // the backup and its one-time secret had been built, and reported only
  // "Backup failed: can't access property slice". The server shape is fixed;
  // this holds the client to failing safely rather than crashing if any
  // identity response ever drifts again.
  it("does not crash when the stored identity carries no fingerprint", async () => {
    const user = userEvent.setup();
    getJSON.mockImplementation((url: string) => {
    if (url === "/api/pgp/bootstrap") return Promise.resolve(SESSION.bootstrap);
    if (url === "/api/pgp/identity/envelope/recovery") return Promise.resolve({ envelope: JSON.stringify(BACKUP.envelope), fingerprint: BACKUP.fingerprint, publicKey: BACKUP.publicKey });
      if (url === "/api/mfa/status") {
        return Promise.resolve({
          totpEnabled: true,
          recoveryCodesRemaining: 8,
          pushMfaEnabled: false,
          approverDevices: []
        });
      }
      if (url === "/api/pgp/identity") {
        // The old, wrong shape: an identity object with no `fingerprint`.
        return Promise.resolve({ pgpFingerprint: "ABCDEF0123456789", pgpKeyId: "0123456789" });
      }
      if (url.startsWith("/api/pgp/discovery/suppressions")) {
        return Promise.resolve({ suppressions: [] });
      }
      if (url.startsWith("/api/notifications/native/devices")) {
        return Promise.resolve({ devices: [], deliveryMode: "push" });
      }
      if (url.startsWith("/api/contacts")) {
        return Promise.resolve([]);
      }
      return Promise.resolve({});
    });

    renderPage("mail");
    await user.click(await screen.findByRole("button", { name: "Download recovery backup" }));

    // The secret still reaches the user, from the bootstrap's fingerprint.
    await screen.findByText("SECRET-ABCD-1234");
    expect(createRecoveryBackup).toHaveBeenCalledWith("ARMORED", { fingerprint: "ABCDEF0123456789", publicKey: "PUB" });
    expect(screen.queryByText(/Backup failed/)).toBeNull();
  });
});

describe("the device list and the identity change", () => {
  // Replacing the identity clears every non-password envelope slot on the
  // server, so a list cached across that change would keep showing devices as
  // able to read mail they can no longer open. Observed here through the
  // null -> identity transition every page load already makes, rather than
  // through the generate flow: "Generate new identity" renders only in
  // MailKeys' NO-identity branch, and storeClientPGPIdentity goes through
  // deriveCredential, which this file does not mock.
  // Rendered on Sign-in deliberately: SecurityPage fetches the device list
  // whatever tab is showing, and Sign-in mounts nothing else that fetches it,
  // so the count below is SecurityPage's own effect and nothing else.
  it("re-reads the devices once the identity's fingerprint arrives", async () => {
    renderPage("signin");

    await waitFor(() => {
      const calls = getJSON.mock.calls.filter((c) =>
        String(c[0]).startsWith("/api/notifications/native/devices")
      );
      expect(calls.length).toBeGreaterThan(1);
    });
  });

  it("reads them once when there is no identity to change", async () => {
    getJSON.mockImplementation((url: string) => {
    if (url === "/api/pgp/bootstrap") return Promise.resolve(SESSION.bootstrap);
    if (url === "/api/pgp/identity/envelope/recovery") return Promise.resolve({ envelope: JSON.stringify(BACKUP.envelope), fingerprint: BACKUP.fingerprint, publicKey: BACKUP.publicKey });
      if (url === "/api/mfa/status") {
        return Promise.resolve({
          totpEnabled: true,
          recoveryCodesRemaining: 8,
          pushMfaEnabled: false,
          approverDevices: []
        });
      }
      if (url === "/api/pgp/identity") return Promise.reject(new Error("none"));
      if (url.startsWith("/api/notifications/native/devices")) {
        return Promise.resolve({ devices: [], deliveryMode: "push" });
      }
      return Promise.resolve({});
    });

    renderPage("signin");
    await waitFor(() => expect(getJSON).toHaveBeenCalledWith("/api/pgp/identity"));

    const calls = getJSON.mock.calls.filter((c) =>
      String(c[0]).startsWith("/api/notifications/native/devices")
    );
    expect(calls).toHaveLength(1);
  });
});

describe("a failed device refetch", () => {
  // Re-homed from DeviceEnrollmentCard, which used to hold this for its own
  // copy of the list. An empty list here is not the reassuring "nothing is
  // paired" story — leaving a stale one would keep asserting that a device can
  // read your encrypted mail after the sealing behind that claim is gone.
  it("empties the list and says why, rather than showing stale enrollment", async () => {
    // The first read lands a device; the refetch the arriving identity triggers
    // fails. The device must not survive it — that is the whole point.
    let deviceReads = 0;
    getJSON.mockImplementation((url: string) => {
    if (url === "/api/pgp/bootstrap") return Promise.resolve(SESSION.bootstrap);
    if (url === "/api/pgp/identity/envelope/recovery") return Promise.resolve({ envelope: JSON.stringify(BACKUP.envelope), fingerprint: BACKUP.fingerprint, publicKey: BACKUP.publicKey });
      if (url === "/api/mfa/status") {
        return Promise.resolve({
          totpEnabled: true,
          recoveryCodesRemaining: 8,
          pushMfaEnabled: false,
          approverDevices: []
        });
      }
      if (url === "/api/pgp/identity") {
        return Promise.resolve({
          pgpRevision: 7,
        fingerprint: "ABCDEF0123456789",
          keyId: "0123456789",
          publicKey: "PUB",
          source: "generated",
          createdAt: "2026-01-01T00:00:00Z"
        });
      }
      if (url.startsWith("/api/notifications/native/devices")) {
        deviceReads += 1;
        if (deviceReads === 1) {
          return Promise.resolve({
            deliveryMode: "push",
            devices: [
              {
                deviceId: "d1",
                platform: "android",
                pushToken: "tok",
                deviceName: "Pixel",
                enrollmentPublicKey: "K",
                encryptionEnrolled: true
              }
            ]
          });
        }
        return Promise.reject(new Error("Could not read your paired devices."));
      }
      if (url.startsWith("/api/pgp/discovery/suppressions")) {
        return Promise.resolve({ suppressions: [] });
      }
      if (url.startsWith("/api/contacts")) return Promise.resolve([]);
      return Promise.resolve({});
    });

    renderPage("devices");

    // It was there first — otherwise its absence below proves nothing.
    expect(await screen.findByText("Pixel")).toBeTruthy();

    expect(await screen.findByText("Could not read your paired devices.")).toBeTruthy();
    expect(screen.queryByText("Pixel")).toBeNull();
    expect(screen.queryByText("This device can read your encrypted mail.")).toBeNull();

    // The assertion that actually pins the CLEARING rather than the error
    // branch: the tab swaps the list for the error message either way, so only
    // the page-level summary — which renders regardless — can tell "the list
    // was emptied" from "the list is merely hidden behind an error".
    await waitFor(() => expect(screen.getByText("None paired")).toBeTruthy());
  });
});

describe("the encryption summary", () => {
  function withDevices(devices: unknown[]) {
    getJSON.mockImplementation((url: string) => {
    if (url === "/api/pgp/bootstrap") return Promise.resolve(SESSION.bootstrap);
    if (url === "/api/pgp/identity/envelope/recovery") return Promise.resolve({ envelope: JSON.stringify(BACKUP.envelope), fingerprint: BACKUP.fingerprint, publicKey: BACKUP.publicKey });
      if (url === "/api/mfa/status") {
        return Promise.resolve({
          totpEnabled: true,
          recoveryCodesRemaining: 8,
          pushMfaEnabled: false,
          approverDevices: []
        });
      }
      if (url === "/api/pgp/identity") {
        return Promise.resolve({
          pgpRevision: 7,
        fingerprint: "ABCDEF0123456789",
          keyId: "0123456789",
          publicKey: "PUB",
          source: "generated",
          createdAt: "2026-01-01T00:00:00Z"
        });
      }
      if (url.startsWith("/api/notifications/native/devices")) {
        return Promise.resolve({ devices, deliveryMode: "push" });
      }
      if (url.startsWith("/api/pgp/discovery/suppressions")) {
        return Promise.resolve({ suppressions: [] });
      }
      if (url.startsWith("/api/contacts")) return Promise.resolve([]);
      return Promise.resolve({});
    });
  }

  const dev = (id: string, enrolled: boolean) => ({
    deviceId: id,
    platform: "android",
    pushToken: "tok",
    deviceName: id,
    enrollmentPublicKey: "K",
    encryptionEnrolled: enrolled
  });

  // "Only this browser" stops being true the moment a device is enrolled —
  // enrolling is precisely the act of giving something other than this browser
  // a copy of the key.
  it("counts the devices that can also read your mail", async () => {
    withDevices([dev("a", true), dev("b", false)]);
    renderPage("devices");

    expect(
      await screen.findByText(
        "Only this browser and 1 of your 2 devices can open mail encrypted to you."
      )
    ).toBeTruthy();
  });

  it("says only this browser when nothing is enrolled", async () => {
    withDevices([dev("a", false), dev("b", false)]);
    renderPage("devices");

    expect(
      await screen.findByText("Only this browser can open mail encrypted to you.")
    ).toBeTruthy();
  });
});

describe("SIBLING: recovery codes survive a tab switch", () => {
  it("keeps just-issued recovery codes across Sign-in -> Devices -> Sign-in", async () => {
    const user = userEvent.setup();
    getJSON.mockImplementation((url: string) => {
    if (url === "/api/pgp/bootstrap") return Promise.resolve(SESSION.bootstrap);
    if (url === "/api/pgp/identity/envelope/recovery") return Promise.resolve({ envelope: JSON.stringify(BACKUP.envelope), fingerprint: BACKUP.fingerprint, publicKey: BACKUP.publicKey });
      if (url === "/api/mfa/status") {
        return Promise.resolve({
          totpEnabled: true,
          recoveryCodesRemaining: 8,
          pushMfaEnabled: false,
          approverDevices: []
        });
      }
      if (url === "/api/pgp/identity") return Promise.reject(new Error("none"));
      if (url.startsWith("/api/notifications/native/devices")) {
        return Promise.resolve({ devices: [], deliveryMode: "push" });
      }
      return Promise.resolve({});
    });
    postJSON.mockImplementation((url: string) => {
      if (url === "/api/mfa/recovery-codes/regenerate") {
        return Promise.resolve({ ok: true, recoveryCodes: ["CODE-AAAA", "CODE-BBBB"] });
      }
      return Promise.resolve({});
    });

    renderPage("signin");
    await user.click(await screen.findByRole("button", { name: "Regenerate recovery codes" }));
    const pw = document.querySelector("form.sec-inline-form input[type=password]") as HTMLInputElement;
    await user.type(pw, "hunter2");
    await user.click(screen.getByRole("button", { name: "Regenerate" }));

    await screen.findByText("CODE-AAAA");

    await user.click(screen.getByRole("tab", { name: "Devices" }));
    await user.click(screen.getByRole("tab", { name: "Sign-in" }));
    await waitFor(() => expect(screen.queryByText("CODE-AAAA")).not.toBeNull());
  });
});

describe("SIBLING: TOTP enrollment secret across a tab switch", () => {
  it("keeps the scanned setup secret across Sign-in -> Devices -> Sign-in", async () => {
    const user = userEvent.setup();
    getJSON.mockImplementation((url: string) => {
    if (url === "/api/pgp/bootstrap") return Promise.resolve(SESSION.bootstrap);
    if (url === "/api/pgp/identity/envelope/recovery") return Promise.resolve({ envelope: JSON.stringify(BACKUP.envelope), fingerprint: BACKUP.fingerprint, publicKey: BACKUP.publicKey });
      if (url === "/api/mfa/status") {
        return Promise.resolve({
          totpEnabled: false,
          recoveryCodesRemaining: 0,
          pushMfaEnabled: false,
          approverDevices: []
        });
      }
      if (url === "/api/pgp/identity") return Promise.reject(new Error("none"));
      if (url.startsWith("/api/notifications/native/devices")) {
        return Promise.resolve({ devices: [], deliveryMode: "push" });
      }
      return Promise.resolve({});
    });
    postJSON.mockImplementation((url: string) => {
      if (url === "/api/mfa/totp/setup") {
        return Promise.resolve({ secret: "TOTPSECRET123", otpauthUri: "otpauth://totp/x?secret=TOTPSECRET123" });
      }
      return Promise.resolve({});
    });

    renderPage("signin");
    await user.click(await screen.findByRole("button", { name: "Enable 2FA" }));
    await screen.findByText("TOTPSECRET123");

    await user.click(screen.getByRole("tab", { name: "Devices" }));
    await user.click(screen.getByRole("tab", { name: "Sign-in" }));

    // The secret is still there — a naive fix would need a second
    // POST /api/mfa/totp/setup to show something here, and that call mints
    // a DIFFERENT secret, orphaning whatever the user already scanned into
    // their authenticator app.
    await screen.findByText("TOTPSECRET123");
    expect(postJSON).toHaveBeenCalledTimes(1);
  });
});

describe("no two page-level status regions at once", () => {
  it("shows one message region, from either tab", async () => {
    const user = userEvent.setup();
    putJSON.mockRejectedValue(new Error("boom"));
    renderPage("devices");
    await screen.findByRole("button", { name: "App Pull" });
    await user.click(screen.getByRole("button", { name: "App Pull" }));
    await waitFor(() => {
      const notices = document.querySelectorAll(".notice");
      expect(notices.length).toBe(1);
    });
  });
});

// The CardDAV app password is generated once and is not re-fetchable, so
// unmounting the Devices tab while one is on screen destroys the only copy.
// The guard that blocks the tab switch used to live on the Configuration page;
// it moved here with the section, and this pins that it actually came along —
// without it, one stray tab click silently loses the password.
describe("CardDAV password blocks tab switches", () => {
  it("refuses to leave the CardDAV tab while a generated password is showing", async () => {
    const user = userEvent.setup();
    postJSON.mockImplementation((url: string) => {
      if (url === "/api/contacts/dav-password") {
        return Promise.resolve({ password: "generated-app-password" });
      }
      return Promise.resolve({ ok: true });
    });

    renderPage("carddav");

    const generate = await screen.findByRole("button", { name: /generate/i });
    await user.click(generate);
    await screen.findByText("generated-app-password");

    await user.click(screen.getByRole("tab", { name: "Encryption" }));

    expect(screen.getByRole("alert").textContent).toContain("before switching tabs");
    expect(screen.getByRole("tab", { name: "CardDAV" }).getAttribute("aria-selected")).toBe("true");
    expect(screen.getByText("generated-app-password")).toBeTruthy();
  });
});

describe("server recovery", () => {
  it("uploads only the envelope with derived step-up and the validated identity", async () => {
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Download recovery backup" }));
    expect(putJSON).not.toHaveBeenCalled();
    await userEvent.click(await screen.findByRole("button", { name: "I saved the secret — store server copy" }));
    await waitFor(() => expect(putJSON).toHaveBeenCalledWith("/api/pgp/identity/envelope/recovery", {
      envelope: JSON.stringify(BACKUP.envelope), expectedFingerprint: BACKUP.fingerprint, expectedRevision: 7, authSecret: "derived-account-credential"
    }));
    expect(JSON.stringify(putJSON.mock.calls)).not.toMatch(/SECRET-ABCD|ARMORED|account-password/);
  });

  it("retains the file and secret after an uncertain upload and a tab switch", async () => {
    putJSON.mockRejectedValue(new Error("connection lost"));
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Download recovery backup" }));
    await userEvent.click(await screen.findByRole("button", { name: "I saved the secret — store server copy" }));
    await screen.findByText(/Server recovery storage could not be confirmed/);
    await userEvent.click(screen.getByRole("tab", { name: "Devices" }));
    await userEvent.click(screen.getByRole("tab", { name: "Encryption" }));
    expect(await screen.findByText("SECRET-ABCD-1234")).toBeTruthy();
    expect(screen.getByRole("button", { name: "Download file again" })).toBeTruthy();
    expect(createRecoveryBackup).toHaveBeenCalledTimes(1);
  });

  it("refuses to back up a stale unlocked key", async () => {
    importIdentity.mockResolvedValue({ fingerprint: "OTHER", armoredPrivateKey: "OTHER-PRIVATE" });
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Download recovery backup" }));
    await screen.findByText(/Your PGP identity changed/);
    expect(createRecoveryBackup).not.toHaveBeenCalled();
    expect(putJSON).not.toHaveBeenCalled();
  });

  async function openCopy() {
    SESSION.unlocked = false;
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Use server recovery copy" }));
    await screen.findByRole("heading", { name: "Server recovery copy" });
    await userEvent.type(screen.getByLabelText("Recovery secret"), "SECRET-ABCD-1234");
  }

  it("restores from the server with a locked vault and no downloaded file", async () => {
    await openCopy();
    await userEvent.type(screen.getByLabelText("Current account password (restore only)"), "account-password");
    await userEvent.click(screen.getByRole("button", { name: "Restore" }));
    await screen.findByText(/PGP key restored and re-encrypted/);
    expect(postJSON).toHaveBeenCalledWith("/api/pgp/identity/rewrap", {
      wrapped: "{}", expectedFingerprint: BACKUP.fingerprint, expectedRevision: 7, authSecret: "derived-account-credential"
    });
    expect(unlockWithArmoredKey).toHaveBeenCalledWith("ARMORED");
  });

  it("drills without unlocking or rewrapping and records only a date and ciphertext hash", async () => {
    await openCopy();
    await userEvent.click(screen.getByRole("button", { name: "Run recovery drill" }));
    await screen.findByText(/Recovery drill passed. The key opened/);
    expect(postJSON).not.toHaveBeenCalled();
    expect(putJSON).not.toHaveBeenCalled();
    expect(unlockWithArmoredKey).not.toHaveBeenCalled();
    const raw = localStorage.getItem(`kypost-pgp-drill:user-1:${BACKUP.fingerprint}`);
    expect(raw).toBeTruthy();
    expect(raw).not.toMatch(/ARMORED|SECRET|PUB|salt|ciphertext/);
    expect(Object.keys(JSON.parse(raw!)).sort()).toEqual(["date", "hash"]);
    expect((screen.getByLabelText("Recovery secret") as HTMLInputElement).value).toBe("");
  });

  it.each(["wrong secret", "different identity"])("does not record a failed drill: %s", async (reason) => {
    if (reason === "wrong secret") restoreRecoveryBackup.mockRejectedValueOnce(new Error("wrong secret"));
    else importIdentity.mockResolvedValueOnce({ fingerprint: "OTHER" });
    await openCopy();
    await userEvent.click(screen.getByRole("button", { name: "Run recovery drill" }));
    await screen.findByText(/Drill failed:/);
    expect(localStorage.length).toBe(0);
    expect(postJSON).not.toHaveBeenCalled();
    expect(unlockWithArmoredKey).not.toHaveBeenCalled();
  });

  it("always explains that identity deletion removes server recovery too", async () => {
    SESSION.bootstrap.envelopeSlots = ["password", "recovery"];
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Delete identity" }));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("also deletes the server recovery copy"));
    expect(confirm).toHaveBeenCalledWith(expect.stringContaining("downloaded recovery file and its secret"));
    expect(deleteJSON).not.toHaveBeenCalled();
  });
});


it("cannot replace server recovery when the user leaves before saving the secret", async () => {
  const page = renderPage();
  await userEvent.click(await screen.findByRole("button", { name: "Download recovery backup" }));
  await screen.findByText("SECRET-ABCD-1234");
  page.unmount();
  expect(putJSON).not.toHaveBeenCalled();
});

it("starts a delayed recovery PUT only after secret-save acknowledgement", async () => {
  let finish: () => void = () => {};
  putJSON.mockImplementation(() => new Promise<void>((resolve) => { finish = resolve; }));
  const page = renderPage();
  await userEvent.click(await screen.findByRole("button", { name: "Download recovery backup" }));
  await screen.findByText("SECRET-ABCD-1234");
  expect(putJSON).not.toHaveBeenCalled();
  await userEvent.click(screen.getByRole("button", { name: "I saved the secret — store server copy" }));
  await waitFor(() => expect(putJSON).toHaveBeenCalledTimes(1));
  page.unmount();
  finish();
});


it("retains a prepared recovery revision across refresh, tab switch and rejected upload", async () => {
  renderPage();
  await userEvent.click(await screen.findByRole("button", { name: "Download recovery backup" }));
  await screen.findByText("SECRET-ABCD-1234");
  SESSION.bootstrap.pgpRevision = 8;
  await userEvent.click(screen.getByRole("tab", { name: "Devices" }));
  await userEvent.click(screen.getByRole("tab", { name: "Encryption" }));
  putJSON.mockRejectedValue(new Error("PGP state changed; reload"));
  await userEvent.click(await screen.findByRole("button", { name: "I saved the secret — store server copy" }));
  await screen.findByText(/PGP state changed/);
  expect(putJSON).toHaveBeenCalledExactlyOnceWith("/api/pgp/identity/envelope/recovery", expect.objectContaining({ expectedRevision: 7 }));
  expect(screen.getByText("SECRET-ABCD-1234")).toBeTruthy();
  expect(createRecoveryBackup).toHaveBeenCalledTimes(1);
});


describe("complete keyring recovery UI gates", () => {
  const fingerprint = "A".repeat(40);
  const keyring: KeyringMetadata = { version: 1, materialGeneration: 1, primaryFingerprints: [fingerprint], keyFingerprints: [fingerprint] };
  const ringBackup = { ...BACKUP, format: "kypost-pgp-recovery-v2" as const, keyring };
  async function openRing() {
    SESSION.bootstrap.keyring = keyring;
    restoreRecoveryBackup.mockResolvedValue({ ...ringBackup, privateKey: "RING" });
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Use server recovery copy" }));
    await screen.findByRole("heading", { name: "Server recovery copy" });
    await userEvent.type(screen.getByLabelText("Recovery secret"), "SECRET");
  }
  it("drills the entire ring without invoking a single-key parser, writer or vault unlock", async () => {
    await openRing();
    await userEvent.click(screen.getByRole("button", { name: "Run recovery drill" }));
    await screen.findByText(/Every retained key and the current public identity match/);
    expect(validateKeyringSnapshot).toHaveBeenCalledWith("RING", expect.objectContaining({ keyring }));
    expect(importIdentity).not.toHaveBeenCalled();
    expect(postJSON).not.toHaveBeenCalled();
    expect(putJSON).not.toHaveBeenCalled();
    expect(unlockWithArmoredKey).not.toHaveBeenCalled();
  });
  it("does not install a ring or claim successful restoration before lifecycle uploads exist", async () => {
    await openRing();
    await userEvent.type(screen.getByLabelText("Current account password (restore only)"), "account-password");
    await userEvent.click(screen.getByRole("button", { name: "Restore" }));
    await screen.findByText(/Complete keyring restoration requires the lifecycle update/);
    expect(postJSON).not.toHaveBeenCalled();
    expect(putJSON).not.toHaveBeenCalled();
    expect(unlockWithArmoredKey).not.toHaveBeenCalled();
    expect(localStorage.length).toBe(0);
  });
  it("checks the snapshot fetched after decryption, rejecting a changed ring", async () => {
    await openRing();
    restoreRecoveryBackup.mockImplementationOnce(async () => {
      SESSION.bootstrap.keyring = { ...keyring, materialGeneration: 2 };
      return { ...ringBackup, privateKey: "RING" };
    });
    validateKeyringSnapshot.mockRejectedValueOnce(new Error("current complete keyring changed"));
    await userEvent.click(screen.getByRole("button", { name: "Run recovery drill" }));
    await screen.findByText(/Drill failed: current complete keyring changed/);
    expect(validateKeyringSnapshot).toHaveBeenCalledWith("RING", expect.objectContaining({ keyring: { ...keyring, materialGeneration: 2 } }));
    expect(localStorage.length).toBe(0);
    expect(unlockWithArmoredKey).not.toHaveBeenCalled();
  });
  it("rejects a legacy backup for a converted account before any single-key import", async () => {
    await openRing();
    restoreRecoveryBackup.mockResolvedValueOnce({ ...BACKUP, privateKey: "ARMORED" });
    await userEvent.click(screen.getByRole("button", { name: "Run recovery drill" }));
    await screen.findByText(/A legacy backup cannot replace retained keys/);
    expect(importIdentity).not.toHaveBeenCalled();
    expect(localStorage.length).toBe(0);
    expect(postJSON).not.toHaveBeenCalled();
  });
  it("offers a complete offline download and never the legacy slot upload", async () => {
    SESSION.bootstrap.keyring = keyring;
    createRecoveryBackup.mockResolvedValueOnce({ backup: ringBackup, secret: "SECRET-ABCD-1234" });
    renderPage();
    await userEvent.click(await screen.findByRole("button", { name: "Download recovery backup" }));
    await screen.findByText(/Complete keyring recovery file checked/);
    expect(createRecoveryBackup).toHaveBeenCalledWith("RING", SESSION.bootstrap);
    expect(importIdentity).not.toHaveBeenCalled();
    expect(screen.queryByRole("button", { name: /store server copy/ })).toBeNull();
    expect(putJSON).not.toHaveBeenCalled();
  });
});
