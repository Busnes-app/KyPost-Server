import { useEffect, useState } from "react";
import { toErrorMessage } from "../../api/client";
import { confirmSendAsAlias, createSendAsAlias, deleteSendAsAlias, listSendAsAliases, type SendAsAlias } from "../../api/sendas";
import { formatWhen, sendAsStatusClass, sendAsStatusLabel } from "../../pages/config/settings";

export function SendAs() {
  const [sendAsAliases, setSendAsAliases] = useState<SendAsAlias[]>([]);
  const [sendAsEmail, setSendAsEmail] = useState("");
  const [sendAsDisplayName, setSendAsDisplayName] = useState("");
  const [sendAsMessage, setSendAsMessage] = useState("");
  const [sendAsBusy, setSendAsBusy] = useState(false);
  const [codes, setCodes] = useState<Record<string, string>>({});

  async function refreshSendAsAliases() {
    setSendAsAliases(await listSendAsAliases());
  }

  useEffect(() => {
    void refreshSendAsAliases().catch(() => undefined);
  }, []);

  // While any alias is still verifying, poll for status changes so the list
  // updates on its own if the daemon's DKIM loop-back check verifies it before
  // the user types the code. Stops as soon as nothing is pending.
  useEffect(() => {
    if (!sendAsAliases.some((alias) => alias.status === "pending")) {
      return;
    }
    const interval = window.setInterval(() => {
      refreshSendAsAliases().catch(() => undefined);
    }, 15000);
    return () => window.clearInterval(interval);
  }, [sendAsAliases]);

  async function addSendAsAlias() {
    const email = sendAsEmail.trim();
    if (!email) {
      setSendAsMessage("Enter an email address first.");
      return;
    }
    setSendAsBusy(true);
    setSendAsMessage("");
    try {
      await createSendAsAlias(email, sendAsDisplayName.trim());
      setSendAsEmail("");
      setSendAsDisplayName("");
      setSendAsMessage("Verification email sent to that address. Open its mailbox, copy the code from the message, and enter it below within 30 minutes.");
      await refreshSendAsAliases();
    } catch (error: unknown) {
      setSendAsMessage(`Failed to start verification: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setSendAsBusy(false);
    }
  }

  async function confirmAlias(alias: SendAsAlias) {
    const code = (codes[alias.id] ?? "").trim();
    if (!code) {
      setSendAsMessage("Enter the code from the verification email first.");
      return;
    }
    setSendAsBusy(true);
    setSendAsMessage("");
    try {
      await confirmSendAsAlias(alias.id, code);
      setCodes((prev) => ({ ...prev, [alias.id]: "" }));
      setSendAsMessage(`${alias.email} is verified for sending.`);
      await refreshSendAsAliases();
    } catch (error: unknown) {
      setSendAsMessage(`Could not confirm: ${toErrorMessage(error, "unknown error")}`);
      await refreshSendAsAliases().catch(() => undefined);
    } finally {
      setSendAsBusy(false);
    }
  }

  async function removeSendAsAlias(alias: SendAsAlias) {
    if (!window.confirm(`Remove ${alias.email} as a send-as address?`)) {
      return;
    }
    setSendAsBusy(true);
    setSendAsMessage("");
    try {
      await deleteSendAsAlias(alias.id);
      await refreshSendAsAliases();
    } catch (error: unknown) {
      setSendAsMessage(`Failed to remove address: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setSendAsBusy(false);
    }
  }

  return (
    <div className="config-card">
      <h3>Send-As Addresses</h3>
      <p className="config-muted">
        Add a secondary email address you also control. KyPost sends a one-time code to that address, as that
        address, through your outgoing mail server. Enter the code here to use it as a From address. If the
        message also lands in this inbox signed by the address&apos;s domain, the address is proven to the
        domain as well and your key can be published for it over WKD.
      </p>
      <div className="config-grid config-grid-two">
        <label>
          <div>Email Address</div>
          <input
            type="email"
            value={sendAsEmail}
            onChange={(event) => setSendAsEmail(event.target.value)}
            placeholder="you@another-domain.com"
          />
        </label>
        <label>
          <div>Display Name (optional)</div>
          <input value={sendAsDisplayName} onChange={(event) => setSendAsDisplayName(event.target.value)} />
        </label>
      </div>
      <div className="config-actions">
        <button type="button" onClick={() => void addSendAsAlias()} disabled={sendAsBusy}>
          {sendAsBusy ? "Working..." : "Verify Address"}
        </button>
      </div>
      {sendAsMessage ? <p className="config-muted">{sendAsMessage}</p> : null}

      {sendAsAliases.length > 0 ? (
        <div className="config-status-card">
          {sendAsAliases.map((alias) => (
            <div
              key={alias.id}
              style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, padding: "6px 0" }}
            >
              <span>
                {alias.displayName ? `${alias.displayName} <${alias.email}>` : alias.email}
                {" — "}
                {alias.status === "verified" && alias.verifiedAt
                  ? alias.verifiedBy === "code"
                    ? `verified for sending ${formatWhen(alias.verifiedAt)}. Not published over WKD: that needs the signed copy to reach this inbox.`
                    : `verified ${formatWhen(alias.verifiedAt)}`
                  : alias.status === "failed"
                    ? `verification failed${alias.failedAt ? ` ${formatWhen(alias.failedAt)}` : ""}`
                    : `awaiting code, expires ${formatWhen(alias.expiresAt)}`}
                {alias.auto ? (
                  <span className="config-muted">
                    {alias.status === "failed"
                      ? " — this is your account address, checked automatically so your public key can be published for it." +
                        " Your key is not being published while this check is failing, which usually means your mail provider" +
                        " does not DKIM-sign the mail you send. KyPost retries weekly."
                      : " — your account address, checked automatically so your public key can be published for it."}
                  </span>
                ) : null}
              </span>
              <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
                {alias.status === "pending" ? (
                  <>
                    <input
                      aria-label={`Verification code for ${alias.email}`}
                      value={codes[alias.id] ?? ""}
                      onChange={(event) => setCodes((prev) => ({ ...prev, [alias.id]: event.target.value }))}
                      placeholder="kp-xxxxxxxx"
                      autoComplete="one-time-code"
                      style={{ width: 130 }}
                    />
                    <button type="button" onClick={() => void confirmAlias(alias)} disabled={sendAsBusy}>
                      Confirm
                    </button>
                  </>
                ) : null}
                <span className={`contacts-badge ${sendAsStatusClass(alias.status)}`}>
                  <span className="contacts-dot" aria-hidden="true" />
                  {sendAsStatusLabel(alias.status)}
                </span>
                <button type="button" onClick={() => void removeSendAsAlias(alias)} disabled={sendAsBusy}>
                  Remove
                </button>
              </span>
            </div>
          ))}
        </div>
      ) : (
        <p className="config-muted">No send-as addresses yet.</p>
      )}
    </div>
  );
}
