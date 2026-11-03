import { useEffect, useState } from "react";
import { toErrorMessage } from "../../api/client";
import { listSendAsAliases } from "../../api/sendas";
import {
  getSignatures,
  MAX_SIGNATURE_NAME,
  MAX_SIGNATURE_TEXT,
  MAX_SIGNATURES,
  putSignatures,
  type SignatureSettings
} from "../../api/signatures";
import { useMailboxes } from "../../app/mailboxes";

// getRandomValues rather than randomUUID: the latter exists only in secure
// contexts, and an operator may serve KyPost over plain HTTP on a LAN.
function newSignatureId(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(12));
  return Array.from(bytes, (b) => b.toString(16).padStart(2, "0")).join("");
}

// The per-address select's value for "follow the default"; never stored, and
// no signature id can contain a colon.
const FOLLOW_DEFAULT = ":default";

/**
 * The account's email signatures: plain text, kept on the server so every
 * KyPost client inserts the same one. Compose picks the From address's own
 * signature, else the default.
 */
export function Signatures() {
  const { mailboxes } = useMailboxes();
  const [settings, setSettings] = useState<SignatureSettings | null>(null);
  const [aliases, setAliases] = useState<string[]>([]);
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let live = true;
    getSignatures()
      .then((loaded) => {
        if (live) setSettings({ ...loaded, signatures: loaded.signatures ?? [], byAddress: loaded.byAddress ?? {} });
      })
      .catch((error: unknown) => {
        if (live) setMessage(`Could not load signatures: ${toErrorMessage(error, "unknown error")}`);
      });
    listSendAsAliases()
      .then((list) => {
        if (live) setAliases(list.filter((a) => a.status === "verified").map((a) => a.email));
      })
      .catch(() => undefined);
    return () => {
      live = false;
    };
  }, []);

  if (!settings) {
    return (
      <div className="config-card">
        <h3>Signatures</h3>
        <p className="config-muted" role="status">{message || "Loading signatures..."}</p>
      </div>
    );
  }

  const addresses = [...new Set([...mailboxes.flatMap((m) => m.addresses), ...aliases, ...Object.keys(settings.byAddress)].map((a) => a.toLowerCase()))];
  const update = (patch: Partial<SignatureSettings>) => setSettings({ ...settings, ...patch });
  const nameOf = (id: string) => settings.signatures.find((s) => s.id === id)?.name || "Untitled";

  function addSignature() {
    if (!settings) return;
    const id = newSignatureId();
    const signatures = [...settings.signatures, { id, name: settings.signatures.length ? "" : "My signature", text: "" }];
    update({ signatures, defaultId: settings.defaultId || id });
  }

  function removeSignature(id: string) {
    if (!settings) return;
    if (!window.confirm(`Delete the signature "${nameOf(id)}"?`)) return;
    const byAddress = Object.fromEntries(Object.entries(settings.byAddress).filter(([, sig]) => sig !== id));
    update({
      signatures: settings.signatures.filter((s) => s.id !== id),
      defaultId: settings.defaultId === id ? "" : settings.defaultId,
      byAddress
    });
  }

  function setForAddress(address: string, value: string) {
    if (!settings) return;
    const byAddress = { ...settings.byAddress };
    if (value === FOLLOW_DEFAULT) delete byAddress[address];
    else byAddress[address] = value;
    update({ byAddress });
  }

  async function save() {
    if (!settings) return;
    const unnamed = settings.signatures.find((s) => !s.name.trim());
    if (unnamed) {
      setMessage("Give every signature a name.");
      return;
    }
    setBusy(true);
    setMessage("");
    try {
      setSettings(await putSignatures(settings));
      setMessage("Signatures saved.");
    } catch (error: unknown) {
      setMessage(`Could not save signatures: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="config-card signatures-settings">
      <h3>Signatures</h3>
      <p className="config-muted">
        Plain text added below what you write. Saved to your account, so the KyPost apps on your other devices use the
        same signatures.
      </p>

      {settings.signatures.length === 0 ? <p className="config-muted">You have no signatures yet.</p> : null}
      {settings.signatures.map((sig, index) => (
        <fieldset key={sig.id} className="signature-item">
          <legend className="visually-hidden">{`Signature ${index + 1}`}</legend>
          <label>
            <div>Name</div>
            <input
              value={sig.name}
              maxLength={MAX_SIGNATURE_NAME}
              onChange={(event) =>
                update({ signatures: settings.signatures.map((s) => (s.id === sig.id ? { ...s, name: event.target.value } : s)) })
              }
            />
          </label>
          <label>
            <div>Signature</div>
            <textarea
              className="config-textarea signature-text"
              rows={4}
              value={sig.text}
              maxLength={MAX_SIGNATURE_TEXT}
              onChange={(event) =>
                update({ signatures: settings.signatures.map((s) => (s.id === sig.id ? { ...s, text: event.target.value } : s)) })
              }
            />
          </label>
          <div className="config-actions">
            <button type="button" onClick={() => removeSignature(sig.id)} disabled={busy}>
              Delete signature
            </button>
          </div>
        </fieldset>
      ))}
      <div className="config-actions">
        <button type="button" onClick={addSignature} disabled={busy || settings.signatures.length >= MAX_SIGNATURES}>
          Add signature
        </button>
      </div>

      <h4>When to add one</h4>
      <div className="config-grid config-grid-two">
        <label>
          <div>Default signature</div>
          <select value={settings.defaultId} onChange={(event) => update({ defaultId: event.target.value })}>
            <option value="">None</option>
            {settings.signatures.map((s) => (
              <option key={s.id} value={s.id}>{s.name || "Untitled"}</option>
            ))}
          </select>
        </label>
        <div className="signature-switches">
          <label className="config-checkbox">
            <input type="checkbox" checked={settings.onReply} onChange={(event) => update({ onReply: event.target.checked })} />
            <span>Add to replies</span>
          </label>
          <label className="config-checkbox">
            <input type="checkbox" checked={settings.onForward} onChange={(event) => update({ onForward: event.target.checked })} />
            <span>Add to forwards</span>
          </label>
        </div>
      </div>

      {addresses.length > 0 ? (
        <>
          <h4>By address</h4>
          <p className="config-muted">Mail sent from one of these addresses can use its own signature instead of the default.</p>
          <div className="config-grid config-grid-two">
            {addresses.map((address) => {
              const value = Object.prototype.hasOwnProperty.call(settings.byAddress, address) ? settings.byAddress[address] : FOLLOW_DEFAULT;
              return (
                <label key={address}>
                  <div className="signature-address">{address}</div>
                  <select value={value} onChange={(event) => setForAddress(address, event.target.value)}>
                    <option value={FOLLOW_DEFAULT}>
                      {settings.defaultId ? `Default (${nameOf(settings.defaultId)})` : "Default (none)"}
                    </option>
                    <option value="">None</option>
                    {settings.signatures.map((s) => (
                      <option key={s.id} value={s.id}>{s.name || "Untitled"}</option>
                    ))}
                  </select>
                </label>
              );
            })}
          </div>
        </>
      ) : null}

      <div className="config-actions">
        <button type="button" className="btn-primary" onClick={() => void save()} disabled={busy}>
          {busy ? "Saving..." : "Save signatures"}
        </button>
      </div>
      <p className="config-muted" role="status">{message}</p>
    </div>
  );
}
