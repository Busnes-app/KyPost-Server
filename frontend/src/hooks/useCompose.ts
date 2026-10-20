import { useEffect, useRef, useState, type ChangeEvent } from "react";
// Quill (and its theme CSS) is the editor for one dialog, but a static import
// puts it in the entry chunk, so the login page pays for it too. loadQuill
// below fetches it when compose first opens.
import type Quill from "quill";
import { mailboxHeaders, postJSON, selectedMailboxId, toErrorMessage } from "../api/client";
import {
  checkPGPRecipients,
  createSealedPickup,
  getPGPDiscoverySettings,
  resolveRecipientKeys,
  sendClientEncryptedMail,
  type DiscoverySettings,
  type PGPRecipientTier
} from "../api/pgp";
import { listSendAsAliases, type SendAsAlias } from "../api/sendas";
import { isDuplicateInField, parseRecipientField, pickupFallbackFlag, serializeRecipientField, splitAddressList } from "../lib/recipients";
import type { RecipientFieldState, RecipientToken } from "../lib/recipients";
import { accountAddress, isClientProtected, needsUnlock, pgpCustody, pgpSessionState, subscribePGPSession } from "../lib/pgpSession";
import { isUnlocked } from "../lib/keyVault";
import { buildEncryptedDeliveries, buildEncryptedDraft, buildEncryptedSentCopy, buildSignedDelivery, encryptedAttachmentBudget, OUTER_PLACEHOLDER_SUBJECT } from "../lib/pgpClient";
import { sealPickup } from "../lib/pickupCrypto";
import { mailboxName, type UserMailbox } from "../app/mailboxes";
import type { ComposeAttachment, DraftComposePayload } from "../app/types";
import {
  MAX_ATTACHMENT_BYTES,
  readFileAsAttachment,
  formatBytes,
  keylessRecipientsFrom409,
  keyChangedRecipientsFrom409,
  deliverSealedPickupLinks,
  combineWarnings,
  secureLinkWarning
} from "../app/compose";
import {
  clearDraftSnapshot,
  hasContent,
  loadDraftSnapshot,
  purgeExpiredDraftSnapshots,
  restoreNotice,
  saveDraftSnapshot
} from "../app/draftAutosave";

// loadQuill pulls the editor and its theme CSS in one chunk. The module is
// cached after the first call, so reopening compose costs nothing.
async function loadQuill(): Promise<typeof Quill> {
  const [quill] = await Promise.all([import("quill"), import("quill/dist/quill.snow.css")]);
  return quill.default;
}

export type Compose = ReturnType<typeof useCompose>;

/** Compose state, autosave, PGP send paths and draft saving; rendered by ComposeWindow. */
export function useCompose({ userId, mailboxes }: { userId?: string; mailboxes: UserMailbox[] }) {
  // Compose stays on the mailbox it was opened in, whatever happens to the selection.
  const [composeMailbox, setComposeMailbox] = useState("");
  const [composeOpen, setComposeOpen] = useState(false);
  const [composeFrom, setComposeFrom] = useState("");
  const [sendAsOptions, setSendAsOptions] = useState<SendAsAlias[]>([]);
  const [composeTo, setComposeTo] = useState<RecipientFieldState>({ tokens: [], draft: "" });
  const [composeCc, setComposeCc] = useState<RecipientFieldState>({ tokens: [], draft: "" });
  const [composeBcc, setComposeBcc] = useState<RecipientFieldState>({ tokens: [], draft: "" });
  const [composeSubject, setComposeSubject] = useState("");
  const [composeHtmlBody, setComposeHtmlBody] = useState("");
  const [composeSending, setComposeSending] = useState(false);
  const [composeUnlockOpen, setComposeUnlockOpen] = useState(false);
  /** A sealed snapshot was found while the vault was locked; restore on unlock. */
  const composeSnapshotPending = useRef(false);
  // Set while this compose window leaves another mailbox's snapshot in place; autosave must not replace it.
  const composeSnapshotKept = useRef(false);
  // Opt-in: send keyless recipients a one-time pickup link rather than
  // failing the send. Off by default because it is weaker than PGP. For
  // client-custody accounts this drives a browser-side sealed-pickup flow;
  // for server-custody accounts it travels to the server as
  // allowPickupFallback on the /api/mail/send body, where it is not merely a
  // client-side branch — the server itself refuses the downgrade unless the
  // flag is set.
  const [composeSendLinkForKeyless, setComposeSendLinkForKeyless] = useState(false);
  const [composeSavingDraft, setComposeSavingDraft] = useState(false);
  const [composeError, setComposeError] = useState("");
  const [composeSuccess, setComposeSuccess] = useState("");
  const [composeNotice, setComposeNotice] = useState("");
  const [composeAttachments, setComposeAttachments] = useState<ComposeAttachment[]>([]);
  const [composeEncrypt, setComposeEncrypt] = useState(false);
  const [composeSign, setComposeSign] = useState(false);
  const composeSignOverridden = useRef(false);
  const [composeMissingKeyRecipients, setComposeMissingKeyRecipients] = useState<string[]>([]);
  const [composeRecipientTiers, setComposeRecipientTiers] = useState<Record<string, PGPRecipientTier>>({});
  const [pgpDiscoverySettings, setPgpDiscoverySettings] = useState<DiscoverySettings | null>(null);
  const [composeEncryptOverridden, setComposeEncryptOverridden] = useState(false);
  const quillEditorRef = useRef<HTMLDivElement | null>(null);
  const quillInstanceRef = useRef<Quill | null>(null);
  const [quillCtor, setQuillCtor] = useState<typeof Quill | null>(null);
  const attachmentInputRef = useRef<HTMLInputElement | null>(null);
  const composeNoticeTimeoutRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Enforce the compose autosave's 24-hour bound. The snapshot is unencrypted
  // plaintext of a message that may have been headed for PGP encryption, and
  // expiring it on read is not an expiry: loadDraftSnapshot runs only when a
  // blank compose window is opened, so a user who closes the tab and never
  // composes again would keep it forever. Swept on mount for the ordinary
  // "opened the app" case, and hourly for a PWA tab that stays open for days
  // without ever reloading. Not scoped to the current user — a shared browser
  // is exactly where a previous account's draft outlives its session.
  useEffect(() => {
    purgeExpiredDraftSnapshots();
    const timer = window.setInterval(purgeExpiredDraftSnapshots, 60 * 60 * 1000);
    return () => window.clearInterval(timer);
  }, []);

  useEffect(() => {
    if (!composeOpen) return;
    return subscribePGPSession((session) => {
      if (session.unlocked && session.bootstrap?.protection === "client" && !composeSignOverridden.current) {
        setComposeSign(true);
      }
      // Locking must leave signing requested: sending then asks for unlock.
    });
  }, [composeOpen]);

  // Fetch the editor the first time compose opens. Landing it in state rather
  // than a ref is what re-runs the effect below, which is otherwise never
  // rendered again in time to build the editor.
  useEffect(() => {
    if (!composeOpen || quillCtor) return;
    // Wrapped in a thunk: a class is a function, and a bare setState(fn)
    // would be taken for an updater and called.
    void loadQuill()
      .then((ctor) => setQuillCtor(() => ctor))
      .catch(() => setComposeError("Could not load the message editor. Reload the page and try again."));
  }, [composeOpen, quillCtor]);

  useEffect(() => {
    if (!composeOpen) return;
    if (!quillEditorRef.current) return;
    if (!quillCtor) return;

    if (quillInstanceRef.current && quillInstanceRef.current.container !== quillEditorRef.current) {
      quillInstanceRef.current = null;
    }

    if (!quillInstanceRef.current) {
      const quill = new quillCtor(quillEditorRef.current, {
        theme: "snow"
      });
      quill.on("text-change", () => {
        setComposeHtmlBody(quill.root.innerHTML);
      });
      quillInstanceRef.current = quill;
    }

    const editor = quillInstanceRef.current;
    if (editor && editor.root.innerHTML !== composeHtmlBody) {
      editor.root.innerHTML = composeHtmlBody;
    }
  }, [composeOpen, composeHtmlBody, quillCtor]);

  function resetComposeForm() {
    setComposeFrom("");
    setComposeTo({ tokens: [], draft: "" });
    setComposeCc({ tokens: [], draft: "" });
    setComposeBcc({ tokens: [], draft: "" });
    setComposeSubject("");
    setComposeHtmlBody("");
    setComposeSending(false);
    setComposeError("");
    setComposeSuccess("");
    if (composeNoticeTimeoutRef.current) {
      clearTimeout(composeNoticeTimeoutRef.current);
      composeNoticeTimeoutRef.current = null;
    }
    setComposeNotice("");
    setComposeAttachments([]);
    setComposeEncrypt(false);
    setComposeEncryptOverridden(false);
    composeSignOverridden.current = false;
    setComposeSign(isClientProtected() && isUnlocked());
    setComposeSendLinkForKeyless(false);
    setComposeMissingKeyRecipients([]);
    setComposeRecipientTiers({});
    if (attachmentInputRef.current) {
      attachmentInputRef.current.value = "";
    }
    if (quillInstanceRef.current) {
      quillInstanceRef.current.setText("");
    }
  }

  async function handleAttachmentPick(event: ChangeEvent<HTMLInputElement>) {
    const files = Array.from(event.target.files ?? []);
    event.target.value = ""; // allow re-picking the same file
    if (files.length === 0) return;
    setComposeError("");
    try {
      const picked = await Promise.all(files.map(readFileAsAttachment));
      setComposeAttachments((current) => {
        const next = [...current, ...picked];
        const total = next.reduce((sum, a) => sum + a.size, 0);
        if (total > MAX_ATTACHMENT_BYTES) {
          setComposeError(`Attachments too large (max ${formatBytes(MAX_ATTACHMENT_BYTES)} total).`);
          return current;
        }
        return next;
      });
    } catch (e) {
      setComposeError(toErrorMessage(e, "failed to read attachment"));
    }
  }

  function removeComposeAttachment(index: number) {
    setComposeAttachments((current) => current.filter((_, i) => i !== index));
  }

  function loadSendAsOptions() {
    listSendAsAliases()
      .then((aliases) => setSendAsOptions(aliases.filter((alias) => alias.status === "verified")))
      .catch(() => setSendAsOptions([]));
  }

  // Debounced autosave of the open compose window. 1s after typing stops is
  // short enough that almost nothing is lost and long enough that a keystroke
  // is not a localStorage write.
  //
  // The Quill editor's live HTML is read here rather than relying on
  // composeHtmlBody: the editor writes through onChange, so on a hard crash
  // the last keystrokes may not have reached React state yet — which is
  // precisely the moment this exists for.
  useEffect(() => {
    if (!composeOpen || !userId || composeSnapshotKept.current) {
      return;
    }
        const timer = setTimeout(() => {
      void saveDraftSnapshot(userId, {
        to: serializeRecipientField(composeTo),
        cc: serializeRecipientField(composeCc),
        bcc: serializeRecipientField(composeBcc),
        subject: composeSubject,
        body: quillInstanceRef.current?.root.innerHTML ?? composeHtmlBody,
        attachments: composeAttachments,
        mailbox: composeMailbox,
        from: composeFrom
      });
    }, 1000);
    return () => clearTimeout(timer);
  }, [composeOpen, userId, composeTo, composeCc, composeBcc, composeSubject, composeHtmlBody, composeAttachments, composeMailbox, composeFrom]);

  // discardComposeDraft clears both the form and the autosaved snapshot. Used
  // wherever the work is finished or deliberately abandoned; closing the
  // window is NOT one of those, so it does not call this.
  function discardComposeDraft() {
    if (userId) {
      clearDraftSnapshot(userId);
    }
    resetComposeForm();
  }

  const mailboxOf = (id: string) => mailboxes.find((m) => (id ? m.id === id : m.kind === "primary"));
  const composeFromChoices = mailboxOf(composeMailbox)?.addresses ?? [];

  /** Pins compose to the selected mailbox, sending from its primary address. */
  function pinComposeMailbox(id = selectedMailboxId()) {
    setComposeMailbox(id);
    setComposeFrom(mailboxOf(id)?.addresses[0] ?? "");
  }

  // Compose opened before the list arrived has no From yet.
  useEffect(() => {
    if (composeOpen && !composeFrom) setComposeFrom(mailboxOf(composeMailbox)?.addresses[0] ?? "");
  }, [mailboxes]);

  function openComposeWindow() {
    composeSnapshotKept.current = false;
    resetComposeForm();
    pinComposeMailbox();
    setComposeError("");
    setComposeSuccess("");
    setComposeNotice("");
    setComposeOpen(true);
    loadSendAsOptions();
    // Recover anything the last session left behind. Only on a blank compose:
    // openDraftInCompose has explicit content and must never be overwritten by
    // a stale snapshot.
    void restoreComposeSnapshot();
  }

  /** The From the browser-encrypted paths put on the wire: the chosen
   *  send-as alias, else the account address the bootstrap reported. */
  function clientSenderAddress(): string {
    // The account address is the primary's; an extra mailbox must name its own.
    if (composeMailbox && !composeFrom) throw new Error("Choose a From address for this mailbox.");
    return composeFrom || accountAddress();
  }

  /** True while the compose window holds nothing the user typed. */
  function composeIsBlank(): boolean {
    return !hasContent({
      to: serializeRecipientField(composeTo),
      cc: serializeRecipientField(composeCc),
      bcc: serializeRecipientField(composeBcc),
      subject: composeSubject,
      body: quillInstanceRef.current?.root.innerHTML ?? composeHtmlBody,
      attachments: composeAttachments
    });
  }

  /**
   * Restores the autosaved snapshot into a blank compose window. A sealed
   * snapshot behind a locked vault opens the unlock prompt instead; the
   * dialog's onUnlocked calls this again. Nothing is ever restored over
   * typing: if the user dismissed the prompt and started writing, a later
   * unlock leaves the window alone and the snapshot where it is.
   */
  async function restoreComposeSnapshot() {
    if (!userId) return;
    if (!composeIsBlank()) {
      composeSnapshotPending.current = false;
      return;
    }
    const snapshot = await loadDraftSnapshot(userId);
    if (snapshot === "locked") {
      composeSnapshotPending.current = true;
      setComposeNotice("An unsent draft is waiting. Unlock your PGP key to restore it.");
      setComposeUnlockOpen(true);
      return;
    }
    composeSnapshotPending.current = false;
    if (snapshot && composeIsBlank()) {
      // A draft goes back to the mailbox it was written in, or stays stored until that mailbox is back.
      const box = mailboxOf(snapshot.mailbox);
      if (snapshot.mailbox && !box) {
        composeSnapshotKept.current = true;
        setComposeNotice(`An unsent draft from ${snapshot.from || "another mailbox"} is kept for when that mailbox is available again; this message is not saved automatically.`);
        return;
      }
      pinComposeMailbox(snapshot.mailbox);
      if (box) setComposeFrom(box.addresses.includes(snapshot.from) ? snapshot.from : box.addresses[0] ?? "");
      setComposeTo(parseRecipientField(snapshot.to));
      setComposeCc(parseRecipientField(snapshot.cc));
      setComposeBcc(parseRecipientField(snapshot.bcc));
      setComposeSubject(snapshot.subject);
      setComposeHtmlBody(snapshot.body);
      setComposeNotice(snapshot.mailbox === selectedMailboxId() || !box ? restoreNotice(snapshot) : `${restoreNotice(snapshot)} It was written in ${mailboxName(box)} and sends from there.`);
    }
  }

  function openDraftInCompose(payload: DraftComposePayload) {
    composeSnapshotKept.current = false;
    composeSignOverridden.current = false;
    setComposeSign(isClientProtected() && isUnlocked());
    pinComposeMailbox(payload.mailbox);
    setComposeTo(parseRecipientField(payload.sentTo ?? ""));
    setComposeCc(parseRecipientField(payload.cc ?? ""));
    setComposeBcc(parseRecipientField(payload.bcc ?? ""));
    setComposeSubject(payload.subject ?? "");
    setComposeHtmlBody(payload.body ?? "");
    setComposeAttachments(payload.attachments ?? []);
    setComposeError("");
    setComposeSuccess("");
    setComposeOpen(true);
    loadSendAsOptions();
  }

  function trashComposeDraft() {
    discardComposeDraft();
    setComposeOpen(false);
  }

  function closeComposeWindow() {
    setComposeOpen(false);
    resetComposeForm();
  }

  useEffect(() => {
    if (!composeOpen) return;
    let cancelled = false;
    getPGPDiscoverySettings()
      .then((settings) => {
        if (!cancelled) setPgpDiscoverySettings(settings);
      })
      .catch(() => {
        if (!cancelled) setPgpDiscoverySettings(null);
      });
    return () => {
      cancelled = true;
    };
  }, [composeOpen]);

  // Spec §6: when autoEncryptWhenKeyKnown is on, silently flip Encrypt on once
  // every current recipient already has a usable pinned key. Uses only the
  // contact-data-only recipients-check (no network discovery), never
  // auto-disables, and backs off entirely once the user has touched the
  // checkbox themselves this compose.
  useEffect(() => {
    if (!pgpDiscoverySettings?.autoEncryptWhenKeyKnown) return;
    if (composeEncrypt) return;
    if (composeEncryptOverridden) return;
    const addresses = [composeTo, composeCc, composeBcc]
      .flatMap((f) => [...f.tokens.map((t) => t.email), f.draft])
      .map((a) => a.trim())
      .filter(Boolean);
    if (addresses.length === 0) return;
    let cancelled = false;
    const timeoutId = setTimeout(() => {
      checkPGPRecipients(addresses)
        .then(({ results }) => {
          if (cancelled) return;
          const byAddress = new Map(results.map((r) => [r.address.toLowerCase(), r]));
          const allVerified = addresses.every((addr) => byAddress.get(addr.toLowerCase())?.tier === "verified");
          if (allVerified) {
            setComposeEncrypt(true);
          }
        })
        .catch(() => {
          // Non-fatal: leave encryption off, user can still enable manually.
        });
    }, 300);
    return () => {
      cancelled = true;
      clearTimeout(timeoutId);
    };
  }, [pgpDiscoverySettings, composeEncrypt, composeEncryptOverridden, composeTo, composeCc, composeBcc]);

  useEffect(() => {
    if (!composeEncrypt) {
      setComposeMissingKeyRecipients([]);
      setComposeRecipientTiers({});
      return;
    }
    const addresses = [composeTo, composeCc, composeBcc]
      .flatMap((f) => [...f.tokens.map((t) => t.email), f.draft])
      .map((a) => a.trim())
      .filter(Boolean);
    if (addresses.length === 0) {
      setComposeMissingKeyRecipients([]);
      setComposeRecipientTiers({});
      return;
    }
    let cancelled = false;
    const timeoutId = setTimeout(() => {
      checkPGPRecipients(addresses)
        .then(({ results }) => {
          if (cancelled) return;
          const missing = results.filter((r) => !r.hasKey).map((r) => r.address);
          setComposeMissingKeyRecipients(missing);
          setComposeRecipientTiers(
            Object.fromEntries(results.map((r) => [r.address.toLowerCase(), r.tier ?? "none"]))
          );
        })
        .catch(() => {
          if (!cancelled) {
            setComposeMissingKeyRecipients([]);
            setComposeRecipientTiers({});
          }
        });
    }, 300);
    return () => {
      cancelled = true;
      clearTimeout(timeoutId);
    };
  }, [composeEncrypt, composeTo, composeCc, composeBcc]);

  // Derived, not stored: the wording depends on composeSendLinkForKeyless, which
  // the user can flip without triggering a fresh recipient-key check, so this
  // has to recompute on every render rather than lag behind the checkbox.
  const composeRecipientKeyWarning =
    composeMissingKeyRecipients.length === 0
      ? ""
      : composeSendLinkForKeyless
      ? `No PGP key on file for: ${composeMissingKeyRecipients.join(", ")} — they'll receive a one-time pickup link instead.`
      : `No PGP key on file for: ${composeMissingKeyRecipients.join(", ")} — sending will be refused unless you tick "Secure link if no key".`;

  /**
   * Seals the message for one keyless recipient and emails them a one-time
   * link. The key rides in the link's fragment, which the server never
   * receives on the fetch, so what it stores is ciphertext it cannot open.
   */
  async function sendSealedPickupLink(address: string, body: string) {
    const { sealed, fragmentKey } = await sealPickup({
      subject: composeSubject,
      body,
      mode: "html",
      from: composeFrom || ""
    });
    const created = await createSealedPickup(address, sealed);
    const link = `${created.url}#${fragmentKey}`;
    // The notice itself is ordinary mail: it carries the link, not the
    // message. It goes through the normal send path because there is no key
    // to encrypt it to — that is the whole situation being handled.
    await postJSON<{ ok: boolean }>("/api/mail/send", {
      from: composeFrom,
      to: address,
      subject: "[Encrypted] Email Sent by KyPost",
      body:
        "You've received a message that was sent encrypted.\n\n" +
        "Since there's no PGP key on file for your address, you can read it once at the link below. " +
        "The message is encrypted in your browser using a key contained in the link itself, " +
        "so the server storing it cannot read it.\n\n" +
        link +
        "\n\nThis link expires in 7 days or as soon as it's opened, whichever comes first.",
      mode: "plain",
      encrypt: false,
      sign: false
    }, mailboxHeaders(composeMailbox));
  }

  /**
   * Encrypts and signs in the browser, then posts the ciphertext.
   *
   * Refuses rather than downgrading when a recipient has no usable key. The
   * server-side path falls back to a one-time pickup link for those, but that
   * works by storing the plaintext on the server — exactly what client-side
   * key protection exists to prevent — so it is not available here, and
   * quietly sending in the clear instead would be worse than failing.
   */
  /**
   * Sign without Encrypt: one RFC 3156 multipart/signed delivery to every
   * recipient, built and signed here. Needs no recipient keys, so nothing is
   * resolved and nothing is refused for a missing key. The Sent copy is still
   * encrypted to our own key: the server stores only ciphertext for this
   * account, whatever went out on the wire.
   */
  /** The whole compose state encrypted to the sender's own key, as a draft. */
  function encryptedDraft(to: string, body: string, date?: Date): Promise<string> {
    return buildEncryptedDraft(
      {
        from: clientSenderAddress(),
        to: splitAddressList(to),
        cc: splitAddressList(serializeRecipientField(composeCc)),
        bcc: splitAddressList(serializeRecipientField(composeBcc)),
        subject: composeSubject,
        date
      },
      "text/html; charset=UTF-8",
      body,
      composeAttachments
    );
  }

  /** What a scheduled send adds to its request: the time, and the encrypted
   *  draft it returns to Drafts as when it cannot go out. */
  async function scheduleFields(to: string, body: string, sendAt: Date | undefined, encrypted: boolean) {
    if (!sendAt) return {};
    const fields = { sendAt: sendAt.toISOString() };
    return encrypted ? { ...fields, draftCopy: await encryptedDraft(to, body, sendAt) } : fields;
  }

  async function sendComposeSignedLocally(to: string, body: string, sendAt?: Date): Promise<string> {
    if (needsUnlock()) {
      setComposeUnlockOpen(true);
      throw new Error("Your PGP key is locked — unlock it, then press Send again.");
    }
    const toList = splitAddressList(to);
    const ccList = splitAddressList(serializeRecipientField(composeCc));
    const bccList = splitAddressList(serializeRecipientField(composeBcc));
    const recipients = [...toList, ...ccList, ...bccList];
    if (recipients.length === 0) {
      throw new Error("Nothing to send: no recipients.");
    }
    // One delivery plus the Sent copy (and a scheduled draft) carry the attachments.
    const budget = encryptedAttachmentBudget(sendAt ? 3 : 2);
    const attached = composeAttachments.reduce((sum, a) => sum + a.size, 0);
    if (composeAttachments.length > 0 && attached > budget) {
      throw new Error(`Attachments too large for a signed message: ${formatBytes(attached)} attached, ${formatBytes(budget)} allowed.`);
    }
    const envelope = { from: clientSenderAddress(), to: toList, cc: ccList, subject: composeSubject, date: sendAt };
    const delivery = await buildSignedDelivery(envelope, "text/html; charset=UTF-8", body, recipients, composeAttachments);
    const sentCopy = await buildEncryptedSentCopy(envelope, "text/html; charset=UTF-8", body, true, composeAttachments);
    const result = await sendClientEncryptedMail({
      from: clientSenderAddress(),
      subject: OUTER_PLACEHOLDER_SUBJECT,
      deliveries: [delivery],
      to: toList,
      cc: ccList,
      bcc: bccList,
      sentCopy,
      sentCopyEncrypted: true,
      mode: "html",
      materialGeneration: pgpSessionState().bootstrap?.keyring?.materialGeneration,
      ...(await scheduleFields(to, body, sendAt, true))
    }, mailboxHeaders(composeMailbox));
    return result.warning ?? "";
  }

  async function sendComposeEncryptedLocally(to: string, body: string, sendAt?: Date) {
    if (needsUnlock()) {
      // Open the prompt and stop. The composed body is deliberately not
      // stashed across the unlock: holding it would mean sending whatever was
      // captured at the moment of the first click, not what is on screen when
      // the user actually confirms.
      setComposeUnlockOpen(true);
      throw new Error("Your PGP key is locked — unlock it, then press Send again.");
    }

    const ccList = splitAddressList(serializeRecipientField(composeCc));
    const bccList = splitAddressList(serializeRecipientField(composeBcc));
    const toList = splitAddressList(to);

    const { results } = await resolveRecipientKeys([...toList, ...ccList, ...bccList]);
    const keyFor = new Map(results.filter((r) => r.usable && r.publicKey).map((r) => [r.address.toLowerCase(), r.publicKey!]));
    const missing = [...toList, ...ccList, ...bccList].filter((addr) => !keyFor.has(addr.toLowerCase()));

    // A BROKEN PIN IS NOT A MISSING KEY, and it must not be reported as one.
    //
    // tier "key_changed" means discovery found a key for this address whose
    // fingerprint does not match the one already pinned to their contact —
    // which is what a key rotation looks like, and also what an interception
    // attempt looks like. It was folded into `missing` and shown with the
    // wording for "never had a key", so the user was told nothing had changed
    // when the one thing worth telling them had. With "send a secure link if no
    // key" ticked they silently got a pickup link instead, and the broken pin
    // was never mentioned at all.
    const changed = results.filter((r) => r.tier === "key_changed").map((r) => r.address);
    if (changed.length > 0) {
      throw new Error(
        `The PGP key for ${changed.join(", ")} has CHANGED since you last saved it. That is what a ` +
          "key rotation looks like, and also what an interception attempt looks like. Verify the new " +
          "fingerprint with them out of band, then update their contact in Contacts. Nothing was sent."
      );
    }

    // Recipients with no key get a secure link instead: the message is
    // sealed here under a random key that travels in the link's fragment,
    // so the server stores ciphertext it cannot read. This is weaker than
    // PGP — whoever can read their mail has the key — so it is an explicit
    // choice, never a silent fallback.
    // A secure link is mailed when the message is sent; a frozen job cannot do that.
    if (missing.length > 0 && sendAt) {
      throw new Error(`Scheduled mail cannot use secure links. Add a PGP key for ${missing.join(", ")} in Contacts, or send it now.`);
    }
    if (missing.length > 0 && !composeSendLinkForKeyless) {
      throw new Error(
        `No usable PGP key for: ${missing.join(", ")}. Add their key in Contacts, tick "send a secure link" to send those recipients a one-time encrypted link instead, or turn off encryption.`
      );
    }
    const keyed = [toList, ccList, bccList].map((list) => list.filter((a) => keyFor.has(a.toLowerCase())));
    const [keyedTo, keyedCc, keyedBcc] = keyed;

    const envelope = {
      from: clientSenderAddress(),
      to: keyedTo,
      cc: keyedCc,
      subject: composeSubject,
      date: sendAt
    };
    // To/Cc share one ciphertext; each Bcc gets its own so they never appear
    // in one another's encryption headers.
    const groups = [
      {
        recipients: [...keyedTo, ...keyedCc],
        publicKeys: [...keyedTo, ...keyedCc].map((a) => keyFor.get(a.toLowerCase())!)
      },
      ...keyedBcc.map((addr) => ({ recipients: [addr], publicKeys: [keyFor.get(addr.toLowerCase())!] }))
    ].filter((g) => g.recipients.length > 0);

    // A secure link seals the body only. Dropping the files without a word is
    // what this path used to do for every encrypted send; it must not survive
    // here for the keyless recipients.
    if (missing.length > 0 && composeAttachments.length > 0) {
      throw new Error(
        `Attachments cannot be sent through a secure link. Add a PGP key for ${missing.join(", ")} in Contacts, or remove the attachments.`
      );
    }

    let warning = "";
    if (groups.length > 0) {
      // Every group and the Sent copy carries the attachments in full, so the
      // budget shrinks with each Bcc recipient. Checked before any encryption
      // so the refusal names the limit rather than arriving as a 413.
      const budget = encryptedAttachmentBudget(groups.length + (sendAt ? 2 : 1));
      const attached = composeAttachments.reduce((sum, a) => sum + a.size, 0);
      // Only a send that carries files is measured against the budget.
      if (composeAttachments.length > 0 && attached > budget) {
        throw new Error(
          `Attachments too large for an encrypted message: ${formatBytes(attached)} attached, ${formatBytes(budget)} allowed` +
            (keyedBcc.length > 0 ? ` with ${keyedBcc.length} Bcc recipient(s).` : ".")
        );
      }
      const deliveries = await buildEncryptedDeliveries(envelope, "text/html; charset=UTF-8", body, groups, composeSign, composeAttachments);
      // The Sent copy is encrypted to our own key before it leaves the browser.
      // It used to be the raw composer HTML, which handed the server the
      // cleartext of a message it had just been given only as ciphertext — and
      // the real subject with it. The server refuses an unencrypted copy now,
      // so this is not optional.
      const sentCopy = await buildEncryptedSentCopy(envelope, "text/html; charset=UTF-8", body, composeSign, composeAttachments);
      const result = await sendClientEncryptedMail({
        from: clientSenderAddress(),
        // The real subject travels inside the ciphertext as a protected
        // header, exactly as it does for the deliveries.
        subject: OUTER_PLACEHOLDER_SUBJECT,
        deliveries,
        to: keyedTo,
        cc: keyedCc,
        bcc: keyedBcc,
        sentCopy,
        sentCopyEncrypted: true,
        mode: "html",
        materialGeneration: pgpSessionState().bootstrap?.keyring?.materialGeneration,
        ...(await scheduleFields(to, body, sendAt, true))
      }, mailboxHeaders(composeMailbox));
      warning = result.warning ?? "";
    }

    if (groups.length === 0 && missing.length === 0) {
      throw new Error("Nothing to send: no recipients.");
    }

    // The keyed ciphertext above is already delivered and cannot be recalled,
    // so a link that fails here must not throw: that would report the whole
    // send as failed, and the retry it invites would deliver the keyed copy
    // twice. Every keyless recipient is attempted and the ones that got nothing
    // are named in the warning, which is the only form of this the user can
    // actually act on.
    //
    // The all-keyless case is the exception: nothing has been delivered, so
    // there is nothing to duplicate and a total failure is a real failure.
    const failedLinks = await deliverSealedPickupLinks(missing, (address) =>
      sendSealedPickupLink(address, body)
    );
    if (groups.length === 0 && failedLinks.length === missing.length) {
      throw new Error(
        `Could not send a secure link to any recipient (${failedLinks.join(", ")}). Nothing was sent.`
      );
    }
    return combineWarnings(warning, secureLinkWarning(failedLinks, missing.length));
  }

  /** Sends now, or at sendAt when given. */
  async function sendComposeEmail(sendAt?: Date) {
    if (composeSending) return;
    const to = serializeRecipientField(composeTo);
    if (!to) {
      setComposeError("TO is required.");
      return;
    }
    setComposeSending(true);
    setComposeError("");
    setComposeSuccess("");
    const body = quillInstanceRef.current?.root.innerHTML ?? composeHtmlBody;
    try {
      // A client-protected account's key is not on the server, so the server
      // cannot sign or encrypt on its behalf — the browser does it and posts
      // ciphertext. Falling through to /api/mail/send here would get a 409
      // (the server refuses rather than silently sending in the clear).
      let warning = "";
      if ((composeEncrypt || composeSign) && isClientProtected()) {
        warning = composeEncrypt ? await sendComposeEncryptedLocally(to, body, sendAt) : await sendComposeSignedLocally(to, body, sendAt);
      } else {
        // A client-custody account's drafts are encrypted, so a scheduled
        // plain send carries its returnable draft encrypted too.
        const custody = pgpCustody();
        if (sendAt && custody === "unknown") {
          throw new Error("Your PGP state could not be confirmed, so the message was not scheduled. Reload and try again.");
        }
        if (sendAt && custody === "client" && needsUnlock()) {
          setComposeUnlockOpen(true);
          throw new Error("Your PGP key is locked — unlock it, then schedule again.");
        }
        const scheduled = await scheduleFields(to, body, sendAt, false);
        const pgpDraft = sendAt && custody === "client" ? { pgpDraft: await encryptedDraft(to, body) } : {};
        const result = await postJSON<{ ok: boolean; sentSaved?: boolean; warning?: string }>("/api/mail/send", {
          from: composeFrom,
          to,
          cc: serializeRecipientField(composeCc),
          bcc: serializeRecipientField(composeBcc),
          subject: composeSubject,
          body,
          mode: "html",
          attachments: composeAttachments.map(({ name, mimeType, dataBase64 }) => ({ name, mimeType, dataBase64 })),
          encrypt: composeEncrypt,
          sign: composeSign,
          allowPickupFallback: pickupFallbackFlag(composeEncrypt, composeSendLinkForKeyless),
          ...scheduled,
          ...pgpDraft
        }, mailboxHeaders(composeMailbox));
        warning = result.warning ?? "";
      }
      // A 200 with a warning means the message went out but something after
      // it did not — the Sent copy failed to save, some BCC deliveries
      // failed, or a pickup link could not be delivered. Closing the window
      // on that is how it used to reach nobody. Empty the form first (so the
      // window cannot be used to send the same message twice) and keep it
      // open carrying the warning; a clean send still closes as before.
      discardComposeDraft();
      if (sendAt) {
        setComposeSuccess(`Scheduled for ${sendAt.toLocaleString()}. It is listed under Scheduled until it goes out.`);
      } else if (warning) {
        setComposeNotice(`Sent — ${warning}`);
      } else {
        setComposeOpen(false);
      }
    } catch (e) {
      // Checked BEFORE the keyless case: a changed key must never be described
      // as a missing one, or the user is pointed at the pickup fallback in
      // exactly the situation where mailing plaintext is worst.
      const keyChanged = keyChangedRecipientsFrom409(e);
      const keyless = keylessRecipientsFrom409(e);
      const message =
        keyChanged !== null
          ? `The PGP key on file for ${keyChanged.join(", ")} no longer matches the key now published for that address. This can mean the key was rotated — or substituted. Verify the new fingerprint with them directly, then update the contact. This message was NOT sent, and a pickup link is not offered here.`
          : keyless !== null
            ? `No PGP key on file for: ${keyless.join(", ")}. Tick "Secure link if no key" to send those recipients a one-time pickup link instead (stored on this server in plaintext for 7 days), or remove them from the recipients.`
            : toErrorMessage(e, "failed to send email");
      setComposeError(`Send failed: ${message}`);
    } finally {
      setComposeSending(false);
    }
  }

  async function saveComposeDraft() {
    if (composeSavingDraft) return;
    const to = serializeRecipientField(composeTo);
    if (!to) {
      setComposeError("TO is required.");
      return;
    }
    setComposeSavingDraft(true);
    setComposeError("");
    setComposeSuccess("");
    const body = quillInstanceRef.current?.root.innerHTML ?? composeHtmlBody;
    try {
      const cc = serializeRecipientField(composeCc);
      const bcc = serializeRecipientField(composeBcc);
      // Tri-state on purpose: an unloaded or failed bootstrap must not read
      // as "not client custody" and send the draft in the clear.
      const custody = pgpCustody();
      if (custody === "unknown") {
        throw new Error("Your PGP state could not be confirmed, so the draft was not saved. Reload and try again.");
      }
      if (custody === "client") {
        // A client-custody draft is encrypted to the user's own key before it
        // leaves the browser: the Drafts folder sits on the same IMAP server
        // as the Sent copy, and a plaintext draft there gave that server the
        // text of a message the user was about to encrypt.
        if (needsUnlock()) {
          setComposeUnlockOpen(true);
          throw new Error("Your PGP key is locked — unlock it, then save again.");
        }
        const budget = encryptedAttachmentBudget(2);
        const attached = composeAttachments.reduce((sum, a) => sum + a.size, 0);
        if (composeAttachments.length > 0 && attached > budget) {
          throw new Error(`Attachments too large for an encrypted draft: ${formatBytes(attached)} attached, ${formatBytes(budget)} allowed.`);
        }
        const pgpDraft = await encryptedDraft(to, body);
        // The plaintext fields carry the placeholder and nothing else; the
        // server ignores them for an encrypted draft but must not be handed
        // the real text anyway. To is already in the wrapper's outer headers;
        // Cc and Bcc travel only inside the ciphertext, so they stay here.
        await postJSON<{ ok: boolean }>("/api/mail/draft", {
          to,
          subject: OUTER_PLACEHOLDER_SUBJECT,
          body: "",
          mode: "html",
          attachments: [],
          pgpDraft
        }, mailboxHeaders(composeMailbox));
      } else {
        await postJSON<{ ok: boolean }>("/api/mail/draft", {
          to,
          cc,
          bcc,
          subject: composeSubject,
          body,
          mode: "html",
          attachments: composeAttachments.map(({ name, mimeType, dataBase64 }) => ({ name, mimeType, dataBase64 }))
        }, mailboxHeaders(composeMailbox));
      }
      // The work is now a real IMAP draft, so the local safety net has
      // nothing left to protect. Clear it rather than leave a stale copy to
      // resurrect over the saved one on next open.
      if (userId) {
        clearDraftSnapshot(userId);
      }
      setComposeSuccess("Draft saved.");
    } catch (e) {
      setComposeError(`Draft not saved: ${toErrorMessage(e, "failed to save draft")}`);
    } finally {
      setComposeSavingDraft(false);
    }
  }

  function addTokenToField(field: "to" | "cc" | "bcc", token: RecipientToken) {
    const setters: Record<"to" | "cc" | "bcc", typeof setComposeTo> = {
      to: setComposeTo,
      cc: setComposeCc,
      bcc: setComposeBcc
    };
    const setField = setters[field];
    setField((prev) => {
      if (isDuplicateInField(prev.tokens, token.email)) {
        if (composeNoticeTimeoutRef.current) {
          clearTimeout(composeNoticeTimeoutRef.current);
        }
        setComposeNotice(`${token.name ?? token.email} is already in ${field.toUpperCase()}.`);
        composeNoticeTimeoutRef.current = setTimeout(() => {
          setComposeNotice("");
          composeNoticeTimeoutRef.current = null;
        }, 3000);
        return prev;
      }
      return { tokens: [...prev.tokens, token], draft: "" };
    });
  }

  return {
    composeOpen, composeSending, composeSavingDraft, composeError, composeSuccess, composeNotice, setComposeNotice,
    composeFrom, setComposeFrom, composeFromChoices, sendAsOptions,
    composeTo, setComposeTo, composeCc, setComposeCc, composeBcc, setComposeBcc, addTokenToField,
    composeSubject, setComposeSubject, composeAttachments, removeComposeAttachment, handleAttachmentPick,
    composeEncrypt, setComposeEncrypt, setComposeEncryptOverridden, composeSign, setComposeSign, composeSignOverridden,
    composeSendLinkForKeyless, setComposeSendLinkForKeyless, composeRecipientTiers, composeRecipientKeyWarning,
    composeUnlockOpen, setComposeUnlockOpen, composeSnapshotPending, restoreComposeSnapshot,
    quillEditorRef, attachmentInputRef,
    openComposeWindow, openDraftInCompose, closeComposeWindow, trashComposeDraft, sendComposeEmail, saveComposeDraft
  };
}
