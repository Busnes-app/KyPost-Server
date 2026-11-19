import { useRef, useState } from "react";
import { ContactPickerModal } from "./ContactPickerModal";
import { PgpUnlockDialog } from "./PgpUnlockDialog";
import { RecipientField } from "./RecipientField";
import { ScheduleDialog } from "./ScheduleDialog";
import { useDialogOpen } from "../hooks/useDialogOpen";
import type { Compose } from "../hooks/useCompose";
import { contactToToken } from "../lib/recipients";
import { isClientProtected } from "../lib/pgpSession";
import { formatBytes } from "../app/compose";
import { CloseIcon } from "../pages/read/icons";
import type { UserMailbox } from "../app/mailboxes";
import { openNewMeeting } from "../lib/meetingLink";
import { ComposeDrive, DriveAttachMenu } from "./ComposeDrive";

/** The compose dialog. All state lives in useCompose, which App owns so pages can open it. */
export function ComposeWindow({ compose, mailboxes, calendarOrigin = "" }: { compose: Compose; mailboxes: UserMailbox[]; calendarOrigin?: string }) {
  const {
    composeOpen, composeSending, composeSavingDraft, composeError, composeSuccess, composeNotice, setComposeNotice, composeAutosaveFailed,
    composeFrom, setComposeFrom, composeFromChoices, composeFromRequired, sendAsOptions,
    composeTo, setComposeTo, composeCc, setComposeCc, composeBcc, setComposeBcc, addTokenToField,
    composeSubject, setComposeSubject, composeAttachments, removeComposeAttachment, handleAttachmentPick,
    composeEncrypt, setComposeEncrypt, setComposeEncryptOverridden, composeSign, setComposeSign, composeSignOverridden,
    composeSendLinkForKeyless, setComposeSendLinkForKeyless, composeRecipientTiers, composeRecipientKeyWarning,
    composeUnlockOpen, setComposeUnlockOpen, composeSnapshotPending, restoreComposeSnapshot,
    quillEditorRef, attachmentInputRef,
    closeComposeWindow, trashComposeDraft, sendComposeEmail, saveComposeDraft, drive
  } = compose;
  const uploading = drive.driveUploads.length > 0;
  const [contactPickerOpen, setContactPickerOpen] = useState(false);
  const [scheduleOpen, setScheduleOpen] = useState(false);
  const composeDialogRef = useRef<HTMLDialogElement | null>(null);
  useDialogOpen(composeDialogRef, composeOpen);

  return (
    <>
      <dialog
        ref={composeDialogRef}
        className="compose-backdrop"
        aria-labelledby="compose-window-title"
        onCancel={(event) => {
          if (composeSending) {
            event.preventDefault();
            return;
          }
          closeComposeWindow();
        }}
        onClick={(event) => {
          if (event.target === composeDialogRef.current && !composeSending) {
            closeComposeWindow();
          }
        }}
      >
          <section
            className={`compose-window${composeSending ? " compose-window-sending" : ""}`}
            onClick={(event) => event.stopPropagation()}
          >
            <div className="compose-topbar">
              <h2 id="compose-window-title">New message</h2>
              <button
                type="button"
                className="compose-close btn-ghost icon-button"
                aria-label="Close"
                title="Close"
                onClick={closeComposeWindow}
                disabled={composeSending || composeSavingDraft}
              >
                <CloseIcon />
              </button>
            </div>
            <p className="compose-connection-hint">Send and Save Draft use this mailbox's mail connection. Check your mail account settings if the connection is unavailable.</p>
            {composeRecipientKeyWarning ? (
              <p className="compose-pgp-warning">{composeRecipientKeyWarning}</p>
            ) : null}

            {composeError ? <p className="notice notice-error" style={{ margin: 0 }}>{composeError}</p> : null}
            <PgpUnlockDialog
              open={composeUnlockOpen}
              reason={composeSnapshotPending.current ? "to restore your unsent draft" : "to sign and encrypt this message"}
              onUnlocked={() => {
                setComposeUnlockOpen(false);
                if (composeSnapshotPending.current) {
                  setComposeNotice("");
                  void restoreComposeSnapshot();
                }
              }}
              onCancel={() => {
                setComposeUnlockOpen(false);
                composeSnapshotPending.current = false;
              }}
            />
            <ScheduleDialog
              open={scheduleOpen}
              onSchedule={(at) => {
                setScheduleOpen(false);
                void sendComposeEmail(at);
              }}
              onCancel={() => setScheduleOpen(false)}
            />
            {composeSuccess ? <p className="notice notice-success" style={{ margin: 0 }}>{composeSuccess}</p> : null}
            {composeNotice ? <p className="notice notice-warning">{composeNotice}</p> : null}
            <div role="status">{composeAutosaveFailed ? <p className="notice notice-warning">Autosave unavailable. Keep this window open and copy your text somewhere safe before leaving.</p> : null}</div>

            <div className="compose-form-grid">
              {mailboxes.length > 1 || composeFromChoices.length > 1 || composeFromRequired ? (
                <label className="compose-field-row">
                  <span>From</span>
                  <select
                    value={composeFrom}
                    onChange={(event) => setComposeFrom(event.target.value)}
                    disabled={composeSending || composeSavingDraft}
                  >
                    {composeFromRequired && !composeFrom ? <option value="" disabled>Choose an address…</option> : null}
                    {composeFromChoices.map((address) => (
                      <option key={address} value={address}>{address}</option>
                    ))}
                  </select>
                </label>
              ) : sendAsOptions.length > 0 ? (
                <label className="compose-field-row">
                  <span>From</span>
                  <select
                    value={composeFrom}
                    onChange={(event) => setComposeFrom(event.target.value)}
                    disabled={composeSending || composeSavingDraft}
                  >
                    <option value="">Default (your account address)</option>
                    {sendAsOptions.map((alias) => (
                      <option key={alias.id} value={alias.email}>
                        {alias.displayName ? `${alias.displayName} <${alias.email}>` : alias.email}
                      </option>
                    ))}
                  </select>
                </label>
              ) : null}
              <div className="compose-field-row">
                <span>To</span>
                <RecipientField
                  label="To"
                  state={composeTo}
                  onDraftChange={(draft) => setComposeTo((prev) => ({ ...prev, draft }))}
                  onAddToken={(token) => addTokenToField("to", token)}
                  onRemoveToken={(index) => setComposeTo((prev) => ({ ...prev, tokens: prev.tokens.filter((_, i) => i !== index) }))}
                  tiers={composeRecipientTiers}
                />
              </div>
              <div className="compose-field-row">
                <span>Cc</span>
                <RecipientField
                  label="Cc"
                  state={composeCc}
                  onDraftChange={(draft) => setComposeCc((prev) => ({ ...prev, draft }))}
                  onAddToken={(token) => addTokenToField("cc", token)}
                  onRemoveToken={(index) => setComposeCc((prev) => ({ ...prev, tokens: prev.tokens.filter((_, i) => i !== index) }))}
                  tiers={composeRecipientTiers}
                />
              </div>
              <div className="compose-field-row">
                <span>Bcc</span>
                <RecipientField
                  label="Bcc"
                  state={composeBcc}
                  onDraftChange={(draft) => setComposeBcc((prev) => ({ ...prev, draft }))}
                  onAddToken={(token) => addTokenToField("bcc", token)}
                  onRemoveToken={(index) => setComposeBcc((prev) => ({ ...prev, tokens: prev.tokens.filter((_, i) => i !== index) }))}
                  tiers={composeRecipientTiers}
                />
              </div>
              <label className="compose-field-row">
                <span>Subject</span>
                <input type="text" value={composeSubject} onChange={(event) => setComposeSubject(event.target.value)} placeholder="Subject" disabled={composeSending || composeSavingDraft} />
              </label>
            </div>

            {composeAttachments.length > 0 ? (
              <div className="compose-attachments">
                {composeAttachments.map((attachment, index) => (
                  <span key={`${attachment.name}-${index}`} className="compose-attachment-chip">
                    <span className="compose-attachment-name">{attachment.name}</span>
                    <span className="compose-attachment-size">({formatBytes(attachment.size)})</span>
                    {drive.driveStatus.available ? (
                      <button
                        type="button"
                        className="compose-attachment-remove"
                        aria-label={`Upload ${attachment.name} to KyDrive and send a link`}
                        title="Upload to KyDrive and send a link"
                        onClick={() => void drive.attachmentToDrive(index)}
                        disabled={composeSending || composeSavingDraft}
                      >
                        Link
                      </button>
                    ) : null}
                    <button
                      type="button"
                      className="compose-attachment-remove"
                      aria-label={`Remove ${attachment.name}`}
                      onClick={() => removeComposeAttachment(index)}
                      disabled={composeSending || composeSavingDraft}
                    >
                      ✕
                    </button>
                  </span>
                ))}
              </div>
            ) : null}
            <ComposeDrive compose={compose} />

            <div
              ref={quillEditorRef}
              className="compose-editor compose-editor-html"
              onKeyDown={(event) => {
                if (event.key === "Escape") {
                  event.preventDefault();
                }
              }}
            />
            <footer className="compose-footer">
            <div className="compose-security" role="group" aria-label="Message security">
                <label style={{ display: "flex", alignItems: "center", gap: 4, fontSize: "0.85rem" }}>
                  <input
                    type="checkbox"
                    checked={composeEncrypt}
                    onChange={(e) => {
                      setComposeEncryptOverridden(true);
                      setComposeEncrypt(e.target.checked);
                    }}
                  />
                  Encrypt
                </label>
                <label style={{ display: "flex", alignItems: "center", gap: 4, fontSize: "0.85rem" }}>
                  <input type="checkbox" checked={composeSign} onChange={(e) => {
                    composeSignOverridden.current = true;
                    setComposeSign(e.target.checked);
                  }} />
                  Sign
                </label>
                {!composeSign ? <span className="ky-status">Unsigned</span> : null}
                {composeEncrypt ? (
                  <label
                    style={{ display: "flex", alignItems: "center", gap: 4, fontSize: "0.85rem" }}
                    title={
                      isClientProtected()
                        ? "Recipients with no PGP key get a one-time link instead. The message is encrypted in your browser " +
                          "and this server only stores ciphertext — but the key is in the link, so anyone who can read that " +
                          "email can read the message. Weaker than PGP."
                        : "Recipients with no PGP key get a one-time link instead. The message is stored on this server in " +
                          "plaintext for 7 days, and the link travels as ordinary unencrypted mail. Weaker than PGP."
                    }
                  >
                    <input
                      type="checkbox"
                      checked={composeSendLinkForKeyless}
                      onChange={(e) => setComposeSendLinkForKeyless(e.target.checked)}
                    />
                    {isClientProtected() ? "Secure link if no key (browser-sealed)" : "Secure link if no key (server-held, 7 days)"}
                  </label>
                ) : null}
                <input
                  ref={attachmentInputRef}
                  type="file"
                  multiple
                  style={{ display: "none" }}
                  onChange={(event) => void handleAttachmentPick(event)}
                />
            </div>
            <div className="compose-actionbar" role="group" aria-label="Message actions">
                <button type="button" className="compose-send btn-primary" onClick={() => void sendComposeEmail()} disabled={composeSending || composeSavingDraft || uploading}>{composeSending ? "Sending..." : "Send"}</button>
                <button type="button" className="compose-save-draft" onClick={() => setScheduleOpen(true)} disabled={composeSending || composeSavingDraft || uploading}>Schedule…</button>
                <button type="button" className="compose-save-draft" onClick={() => void saveComposeDraft()} disabled={composeSending || composeSavingDraft}>Save Draft</button>
                <button type="button" className="compose-attach" onClick={() => attachmentInputRef.current?.click()} disabled={composeSending || composeSavingDraft}>Attach</button>
                {drive.driveStatus.available ? <DriveAttachMenu onPick={(mode) => void drive.attachFromDrive(mode)} disabled={composeSending || composeSavingDraft} /> : null}
                <button type="button" className="compose-attach" onClick={() => setContactPickerOpen(true)} disabled={composeSending || composeSavingDraft}>Contacts</button>
                {calendarOrigin ? (
                  <button type="button" className="compose-attach" onClick={() => openNewMeeting(calendarOrigin, {
                    title: composeSubject,
                    attendees: [...composeTo.tokens, ...composeCc.tokens].map((t) => t.email)
                  })}>New meeting</button>
                ) : null}
                <button type="button" className="compose-trash btn-ghost btn-danger" onClick={trashComposeDraft} disabled={composeSending || composeSavingDraft}>Trash</button>
            </div>
            </footer>
          </section>
      </dialog>
      <ContactPickerModal
        isOpen={contactPickerOpen}
        onClose={() => setContactPickerOpen(false)}
        toTokens={composeTo.tokens}
        ccTokens={composeCc.tokens}
        bccTokens={composeBcc.tokens}
        onAdd={(field, contact) => {
          const token = contactToToken(contact);
          if (token) addTokenToField(field, token);
        }}
      />
    </>
  );
}
