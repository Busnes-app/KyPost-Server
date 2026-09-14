import { beforeEach, describe, expect, it, vi } from "vitest";
import {
  clearDraftSnapshot,
  hasContent,
  loadDraftSnapshot,
  purgeExpiredDraftSnapshots,
  restoreNotice,
  saveDraftSnapshot,
  type DraftInput,
  type DraftSnapshot
} from "./draftAutosave";

// The vault state the module consults, switchable per test. The seal is a
// stand-in for openpgp: reversible, and its output never contains the input.
const vault = vi.hoisted(() => ({ clientProtected: false, locked: false }));
vi.mock("../lib/pgpSession", () => ({
  isClientProtected: () => vault.clientProtected,
  needsUnlock: () => vault.locked
}));
vi.mock("../lib/pgpClient", () => ({
  sealToSelf: async (text: string) => `sealed:${btoa(unescape(encodeURIComponent(text)))}`,
  openSealedToSelf: async (sealed: string) => {
    if (!sealed.startsWith("sealed:")) throw new Error("not sealed");
    return decodeURIComponent(escape(atob(sealed.slice(7))));
  }
}));

/** loadDraftSnapshot without the "locked" arm, for the tests that never lock. */
async function load(userId: string): Promise<DraftSnapshot | null> {
  const got = await loadDraftSnapshot(userId);
  return got === "locked" ? null : got;
}

function draft(over: Partial<DraftInput> = {}): DraftInput {
  return { to: "", cc: "", bcc: "", subject: "", body: "", attachments: [], ...over };
}

const USER = "user-1";

beforeEach(() => {
  window.sessionStorage.clear();
  window.localStorage.clear();
  vault.clientProtected = false;
  vault.locked = false;
});

describe("hasContent", () => {
  it("treats an empty Quill editor as empty", () => {
    // Quill leaves this behind on a blank editor. Counting it as content means
    // every opened compose window overwrites a real snapshot with nothing.
    expect(hasContent(draft({ body: "<p><br></p>" }))).toBe(false);
    expect(hasContent(draft({ body: "<p>&nbsp;</p>" }))).toBe(false);
    expect(hasContent(draft({ body: "<p>&emsp;&#160;</p>" }))).toBe(false);
  });

  it("sees real body text through markup", () => {
    expect(hasContent(draft({ body: "<p>hello</p>" }))).toBe(true);
  });

  it("sees any populated field", () => {
    expect(hasContent(draft({ to: "a@b.test" }))).toBe(true);
    expect(hasContent(draft({ subject: "hi" }))).toBe(true);
    expect(hasContent(draft({ attachments: [{ name: "a.pdf", mimeType: "application/pdf", dataBase64: "x", size: 1 }] }))).toBe(true);
  });
});

describe("save/load round trip", () => {
  it("restores every text field", async () => {
    await saveDraftSnapshot(USER, draft({ to: "a@b.test", cc: "c@d.test", bcc: "e@f.test", subject: "Hi", body: "<p>body</p>" }));
    const got = await load(USER);
    expect(got).toMatchObject({ to: "a@b.test", cc: "c@d.test", bcc: "e@f.test", subject: "Hi", body: "<p>body</p>" });
  });

  it("stores attachment names but never their bytes", async () => {
    await saveDraftSnapshot(
      USER,
      draft({ subject: "x", attachments: [{ name: "report.pdf", mimeType: "application/pdf", dataBase64: "QUJD", size: 3 }] })
    );
    const raw = window.sessionStorage.getItem(`kypost-compose-draft:${USER}`) ?? "";
    expect(raw).toContain("report.pdf");
    // The bytes are what blow the ~5MB quota; they must not be there.
    expect(raw).not.toContain("QUJD");
    expect((await load(USER))?.attachmentNames).toEqual(["report.pdf"]);
  });

  it("clears the stored snapshot when the draft becomes empty", async () => {
    await saveDraftSnapshot(USER, draft({ subject: "typed something" }));
    expect(await load(USER)).not.toBeNull();
    await saveDraftSnapshot(USER, draft());
    expect(await load(USER)).toBeNull();
  });
});

describe("plaintext never becomes persistent", () => {
  // The stored buffer is the plaintext of a message the user may be about to
  // PGP-encrypt. localStorage keeps it on disk until something deletes it —
  // on a shared workstation or a profile backup, that is indefinitely.
  it("writes the draft to sessionStorage and never to localStorage", async () => {
    await saveDraftSnapshot(USER, draft({ subject: "secret", body: "<p>plaintext</p>" }));
    expect(window.sessionStorage.getItem(`kypost-compose-draft:${USER}`)).toContain("secret");
    expect(window.localStorage.getItem(`kypost-compose-draft:${USER}`)).toBeNull();
    expect(window.localStorage.length).toBe(0);
  });

  // Upgrading only helps drafts written after the upgrade unless the old ones
  // go too, and no age makes persistent plaintext of an unsent encrypted
  // message worth keeping.
  it("deletes drafts a previous version left in localStorage, however fresh", () => {
    window.localStorage.setItem(
      `kypost-compose-draft:${USER}`,
      JSON.stringify({ version: 1, subject: "old plaintext", attachmentNames: [], savedAt: new Date().toISOString() })
    );
    purgeExpiredDraftSnapshots();
    expect(window.localStorage.getItem(`kypost-compose-draft:${USER}`)).toBeNull();
  });

  it("leaves unrelated localStorage keys alone while doing it", () => {
    window.localStorage.setItem("kypost-theme", "dark");
    window.localStorage.setItem(`kypost-compose-draft:${USER}`, "{not json");
    purgeExpiredDraftSnapshots();
    expect(window.localStorage.getItem("kypost-theme")).toBe("dark");
    expect(window.localStorage.getItem(`kypost-compose-draft:${USER}`)).toBeNull();
  });
});

describe("isolation and cleanup", () => {
  it("does not leak a draft between accounts on a shared browser", async () => {
    await saveDraftSnapshot(USER, draft({ subject: "private" }));
    expect(await load("user-2")).toBeNull();
  });

  it("clearDraftSnapshot removes it", async () => {
    await saveDraftSnapshot(USER, draft({ subject: "x" }));
    clearDraftSnapshot(USER);
    expect(await load(USER)).toBeNull();
  });

  it("ignores a missing user id rather than writing a shared key", async () => {
    await saveDraftSnapshot("", draft({ subject: "x" }));
    expect(window.sessionStorage.length).toBe(0);
    expect(await load("")).toBeNull();
  });
});

describe("robustness", () => {
  it("returns null for corrupt stored JSON instead of throwing", async () => {
    window.sessionStorage.setItem(`kypost-compose-draft:${USER}`, "{not json");
    expect(await load(USER)).toBeNull();
  });

  it("discards a snapshot from a different version rather than guessing its shape", async () => {
    window.sessionStorage.setItem(`kypost-compose-draft:${USER}`, JSON.stringify({ version: 1, subject: "old", savedAt: new Date().toISOString() }));
    expect(await load(USER)).toBeNull();
  });

  it("does not throw when storage is full", async () => {
    // Swap the whole storage object rather than spying on a method. Neither
    // spy target works in both environments: jsdom hands out a proxied
    // Storage that an instance-level spy does not intercept, while the Node 26
    // polyfill in src/test/setup.ts does not inherit from Storage.prototype,
    // so a prototype spy misses it there. A spy that silently fails to
    // intercept turns this into an assertion that nothing throws when nothing
    // was asked to throw — green, and worthless.
    const original = Object.getOwnPropertyDescriptor(window, "sessionStorage");
    const setItem = vi.fn(() => {
      throw new DOMException("QuotaExceededError");
    });
    Object.defineProperty(window, "sessionStorage", {
      configurable: true,
      value: { ...window.sessionStorage, setItem }
    });

    try {
      // A failed autosave must never surface as an exception mid-typing.
      await expect(saveDraftSnapshot(USER, draft({ subject: "x" }))).resolves.toBeUndefined();
      expect(setItem).toHaveBeenCalled();
    } finally {
      if (original) {
        Object.defineProperty(window, "sessionStorage", original);
      }
    }
  });
});

describe("sealed snapshots on a client-custody account", () => {
  beforeEach(() => {
    vault.clientProtected = true;
  });

  it("stores ciphertext, never the fields, and restores them once opened", async () => {
    await saveDraftSnapshot(USER, draft({ to: "a@b.test", subject: "secret", body: "<p>plaintext</p>" }));
    const raw = window.sessionStorage.getItem(`kypost-compose-draft:${USER}`) ?? "";
    expect(raw).toContain("sealed:");
    expect(raw).not.toContain("secret");
    expect(raw).not.toContain("plaintext");
    expect(raw).not.toContain("a@b.test");
    expect(await load(USER)).toMatchObject({ to: "a@b.test", subject: "secret", body: "<p>plaintext</p>" });
  });

  it("writes nothing while the vault is locked, and keeps what was there", async () => {
    await saveDraftSnapshot(USER, draft({ subject: "before lock" }));
    vault.locked = true;
    await saveDraftSnapshot(USER, draft({ subject: "typed while locked" }));
    // The sealed snapshot is the user's own ciphertext, and the UI has just
    // promised it is recoverable after unlock. Nothing here may destroy it.
    vault.locked = false;
    expect((await load(USER))?.subject).toBe("before lock");
  });

  it("reports locked rather than null when the vault closes, and opens it after unlock", async () => {
    await saveDraftSnapshot(USER, draft({ subject: "secret" }));
    vault.locked = true;
    expect(await loadDraftSnapshot(USER)).toBe("locked");
    // Still there for after the unlock.
    expect(window.sessionStorage.getItem(`kypost-compose-draft:${USER}`)).not.toBeNull();
    vault.locked = false;
    expect((await load(USER))?.subject).toBe("secret");
  });

  it("discards a sealed snapshot the key cannot open", async () => {
    window.sessionStorage.setItem(
      `kypost-compose-draft:${USER}`,
      JSON.stringify({ version: 2, sealed: "not-from-this-key", savedAt: new Date().toISOString() })
    );
    expect(await load(USER)).toBeNull();
    expect(window.sessionStorage.getItem(`kypost-compose-draft:${USER}`)).toBeNull();
  });

  it("still expires by the outer timestamp without opening the seal", async () => {
    window.sessionStorage.setItem(
      `kypost-compose-draft:${USER}`,
      JSON.stringify({ version: 2, sealed: "sealed:e30=", savedAt: new Date(Date.now() - 25 * 60 * 60 * 1000).toISOString() })
    );
    vault.locked = true;
    expect(await loadDraftSnapshot(USER)).toBeNull();
  });
});

describe("expiry", () => {
  // The stored body is the plaintext of a message that may be about to be
  // PGP-encrypted. Logout clears it, but closing the tab or never logging out
  // does not — so it has to expire on its own.
  function storeWithAge(ageMs: number): void {
    window.sessionStorage.setItem(
      `kypost-compose-draft:${USER}`,
      JSON.stringify({
        version: 2,
        fields: { to: "", cc: "", bcc: "", subject: "secret", body: "<p>plaintext</p>", attachmentNames: [] },
        savedAt: new Date(Date.now() - ageMs).toISOString()
      })
    );
  }

  it("still restores a snapshot from within the window", async () => {
    storeWithAge(23 * 60 * 60 * 1000);
    expect((await load(USER))?.subject).toBe("secret");
  });

  it("discards a snapshot older than the window", async () => {
    storeWithAge(25 * 60 * 60 * 1000);
    expect(await load(USER)).toBeNull();
  });

  it("removes the expired plaintext rather than merely refusing to return it", async () => {
    storeWithAge(25 * 60 * 60 * 1000);
    await load(USER);
    expect(window.sessionStorage.getItem(`kypost-compose-draft:${USER}`)).toBeNull();
  });

  it("treats an unparseable savedAt as expired, not as fresh", async () => {
    // Snapshots written before the expiry check existed have no usable
    // timestamp; those are the oldest plaintext on disk, not the newest.
    window.sessionStorage.setItem(
      `kypost-compose-draft:${USER}`,
      JSON.stringify({ version: 2, fields: { subject: "ancient", attachmentNames: [] }, savedAt: "" })
    );
    expect(await load(USER)).toBeNull();
  });
});

describe("restoreNotice", () => {
  it("names attachments that could not be restored", async () => {
    const snap = (await load(USER)) ?? {
      version: 1, to: "", cc: "", bcc: "", subject: "", body: "",
      attachmentNames: ["a.pdf", "b.png"], savedAt: ""
    };
    expect(restoreNotice({ ...snap, attachmentNames: ["a.pdf", "b.png"] })).toContain("a.pdf, b.png");
  });

  it("stays short when there were none", () => {
    expect(restoreNotice({ version: 1, to: "", cc: "", bcc: "", subject: "", body: "", attachmentNames: [], savedAt: "" }))
      .toBe("Restored your unsent draft.");
  });
});

describe("purgeExpiredDraftSnapshots", () => {
  function storeFor(userId: string, ageMs: number, subject = "secret"): void {
    window.sessionStorage.setItem(
      `kypost-compose-draft:${userId}`,
      JSON.stringify({
        version: 2,
        fields: { to: "", cc: "", bcc: "", subject, body: "<p>plaintext</p>", attachmentNames: [] },
        savedAt: new Date(Date.now() - ageMs).toISOString()
      })
    );
  }

  // The bug this sweep exists for. Expiring on read bounds nothing on its own:
  // loadDraftSnapshot is called from exactly one place — opening a BLANK
  // compose window — so a user who closed the tab and never composed again
  // kept the plaintext of a message they may have been about to PGP-encrypt in
  // storage forever, while the code claimed a 24-hour lifetime.
  it("deletes expired plaintext without anyone opening compose", () => {
    storeFor(USER, 25 * 60 * 60 * 1000);
    purgeExpiredDraftSnapshots();
    expect(window.sessionStorage.getItem(`kypost-compose-draft:${USER}`)).toBeNull();
  });

  it("keeps a snapshot inside the window", async () => {
    storeFor(USER, 23 * 60 * 60 * 1000);
    purgeExpiredDraftSnapshots();
    expect((await load(USER))?.subject).toBe("secret");
  });

  // A shared browser is the case the per-user key cannot help with: the other
  // account may never log in again to trigger its own clear.
  it("sweeps every user's snapshot, not just the current one", async () => {
    storeFor("user-1", 25 * 60 * 60 * 1000);
    storeFor("user-2", 25 * 60 * 60 * 1000);
    storeFor("user-3", 1000, "fresh");
    purgeExpiredDraftSnapshots();
    expect(window.sessionStorage.getItem("kypost-compose-draft:user-1")).toBeNull();
    expect(window.sessionStorage.getItem("kypost-compose-draft:user-2")).toBeNull();
    expect((await load("user-3"))?.subject).toBe("fresh");
  });

  it("removes a snapshot whose age cannot be established", () => {
    window.sessionStorage.setItem(
      `kypost-compose-draft:${USER}`,
      JSON.stringify({ version: 1, subject: "ancient", attachmentNames: [] })
    );
    purgeExpiredDraftSnapshots();
    expect(window.sessionStorage.getItem(`kypost-compose-draft:${USER}`)).toBeNull();
  });

  it("removes an unparseable snapshot — unreadable is still plaintext", () => {
    window.sessionStorage.setItem(`kypost-compose-draft:${USER}`, "{not json");
    purgeExpiredDraftSnapshots();
    expect(window.sessionStorage.getItem(`kypost-compose-draft:${USER}`)).toBeNull();
  });

  it("leaves unrelated keys alone", () => {
    window.sessionStorage.setItem("kypost-theme", "dark");
    window.sessionStorage.setItem("unrelated", "keep me");
    storeFor(USER, 25 * 60 * 60 * 1000);
    purgeExpiredDraftSnapshots();
    expect(window.sessionStorage.getItem("kypost-theme")).toBe("dark");
    expect(window.sessionStorage.getItem("unrelated")).toBe("keep me");
  });

  // removeItem reindexes the store, so a sweep that deleted while walking
  // by index would skip every other match.
  it("deletes all expired snapshots when several are adjacent", () => {
    for (let i = 0; i < 6; i++) {
      storeFor(`user-${i}`, 25 * 60 * 60 * 1000);
    }
    purgeExpiredDraftSnapshots();
    for (let i = 0; i < 6; i++) {
      expect(window.sessionStorage.getItem(`kypost-compose-draft:user-${i}`)).toBeNull();
    }
  });

  it("does not throw when storage is unavailable", () => {
    const original = Object.getOwnPropertyDescriptor(window, "sessionStorage")!;
    Object.defineProperty(window, "sessionStorage", {
      configurable: true,
      get() {
        throw new Error("storage disabled");
      }
    });
    try {
      expect(() => purgeExpiredDraftSnapshots()).not.toThrow();
    } finally {
      Object.defineProperty(window, "sessionStorage", original);
    }
  });
});
