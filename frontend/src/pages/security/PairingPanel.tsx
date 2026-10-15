import { useEffect, useState, type FormEvent } from "react";
import QRCode from "qrcode";
import { toErrorMessage } from "../../api/client";
import { mintPairing } from "../../api/devices";
import { useAuth } from "../../auth";
import {
  DEFAULT_PAIRING_TTL_SECONDS,
  QR_CODE_WIDTH_PX,
  clamp,
  pairingBarColor
} from "./format";
import { buildNativePairingLink } from "./pairingLink";

/**
 * "Pair a new device" — the QR code, its expiry bar, and the desktop deep link.
 *
 * A REAL CHILD COMPONENT, mounted only while the panel is open, and that is the
 * point rather than tidiness. `POST /api/notifications/pairing` mints a live
 * 90-second pairing token, and only after the account credential (or a KySignOn
 * confirmation) is re-proved: the device it pairs outlives the session. A code
 * that runs out is not replaced on its own; the user confirms again.
 *
 * (React tracks hook order per component instance, so a conditionally *rendered*
 * child is fine — see the hook rule in frontend/AGENTS.md.)
 */

type NativeDeliveryMode = "push" | "pull";

type PairingStatusResponse = {
  subscriberId: string;
  serverBaseUrl?: string;
  registerEndpoint?: string;
  pullEndpoint?: string;
  deliveryMode?: NativeDeliveryMode;
  subscriberHash?: string;
  pairingToken?: string;
  pairingExpiresAt?: string;
  pairingTtlSeconds?: number;
  /** Leaf SPKI pin of the serving certificate; absent when the server does not terminate TLS. */
  tlsPin?: string;
  configurationError?: string;
  configured: boolean;
};

export type PairingPanelProps = {
  /** Called after a pairing attempt, so the device list can pick up a new one. */
  onDevicesMayHaveChanged: () => void;
  /** Surfaces a message on the page's shared status line. */
  onStatus: (message: string) => void;
};

export function PairingPanel({ onDevicesMayHaveChanged, onStatus }: PairingPanelProps) {
  const { ssoSession = false } = useAuth();
  const [password, setPassword] = useState("");
  const [pairingStatus, setPairingStatus] = useState<PairingStatusResponse | null>(null);
  const [qrDataUrl, setQrDataUrl] = useState("");
  const [expiresAtMs, setExpiresAtMs] = useState<number | null>(null);
  const [ttlMs, setTtlMs] = useState(DEFAULT_PAIRING_TTL_SECONDS * 1000);
  const [clockMs, setClockMs] = useState<number>(() => Date.now());
  const [busy, setBusy] = useState(false);

  function applyPairingStatus(next: PairingStatusResponse | null) {
    setPairingStatus(next);
    if (!next) {
      setExpiresAtMs(null);
      return;
    }

    const ttlSeconds = typeof next.pairingTtlSeconds === "number" && next.pairingTtlSeconds > 0
      ? next.pairingTtlSeconds
      : DEFAULT_PAIRING_TTL_SECONDS;
    setTtlMs(ttlSeconds * 1000);

    // Local clock, not pairingExpiresAt: a fast browser clock would drop the code at once.
    setExpiresAtMs(next.pairingToken ? Date.now() + ttlSeconds * 1000 : null);
    setClockMs(Date.now());
  }

  async function showCode(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setBusy(true);
    const confirm = { ssoSession, password };
    setPassword("");
    try {
      applyPairingStatus(await mintPairing<PairingStatusResponse>(confirm));
    } catch (error: unknown) {
      onStatus(`Could not show a pairing code: ${toErrorMessage(error, "unknown error")}`);
    } finally {
      setBusy(false);
    }
    onDevicesMayHaveChanged();
  }

  // Ticks the bar; an expired code is dropped, never silently replaced.
  useEffect(() => {
    if (!expiresAtMs) {
      return;
    }
    const tick = () => {
      const now = Date.now();
      setClockMs(now);
      if (now >= expiresAtMs) {
        applyPairingStatus(null);
      }
    };
    tick();
    const timer = window.setInterval(tick, 250);
    return () => window.clearInterval(timer);
  }, [expiresAtMs]);

  useEffect(() => {
    let cancelled = false;
    if (!pairingStatus?.configured || !pairingStatus.subscriberId) {
      setQrDataUrl("");
      return;
    }
    QRCode.toDataURL(buildNativePairingLink(pairingStatus), {
      errorCorrectionLevel: "M",
      margin: 2,
      width: QR_CODE_WIDTH_PX
    })
      .then((dataUrl) => {
        if (!cancelled) setQrDataUrl(dataUrl);
      })
      .catch(() => {
        if (!cancelled) setQrDataUrl("");
      });
    return () => {
      cancelled = true;
    };
  }, [pairingStatus]);

  function pairThisDevice(next: PairingStatusResponse) {
    // Desktop apps pair over the same native flow as mobile (sub/hash relay
    // auth): one pairing contract for every client, and the desktop app needs
    // no web session. The link carries the code on screen, which is single-use.
    const deepLink = buildNativePairingLink(next);
    try {
      window.location.href = deepLink;
      onStatus(`Launching desktop app with pairing link (valid for ${Math.round(ttlMs / 1000)} seconds)...`);

      // Fallback: if the desktop app didn't take focus, offer the link for
      // manual pasting into the app's pairing screen.
      setTimeout(() => {
        if (document.hasFocus()) {
          onStatus(`Desktop app not detected. Paste this link into the app's pairing screen: ${deepLink}`);
        }
      }, 2000);
    } catch {
      onStatus(`Desktop app not installed. Pairing link: ${deepLink}`);
    }
  }

  const remainingMs = expiresAtMs ? Math.max(0, expiresAtMs - clockMs) : 0;
  const barWidth = Math.round(QR_CODE_WIDTH_PX * clamp(remainingMs / Math.max(ttlMs, 1), 0, 1));
  const showBar = remainingMs > 0 && pairingStatus?.configured;

  if (!pairingStatus) {
    return (
      <form className="sec-pairing" onSubmit={(event) => void showCode(event)}>
        <p className="sec-muted">
          A paired device keeps access after you sign out, so confirm it is you first. The code
          is good for ninety seconds and works once.
        </p>
        {ssoSession ? (
          <p className="sec-muted">You will confirm with KySignOn.</p>
        ) : (
          <label className="sec-label">
            <span>Account password</span>
            <input
              className="sec-input"
              type="password"
              autoComplete="current-password"
              value={password}
              disabled={busy}
              onChange={(event) => setPassword(event.target.value)}
              required
            />
          </label>
        )}
        <div className="sec-actions">
          <button type="submit" disabled={busy || (!ssoSession && !password)}>
            {busy ? "Confirming…" : "Show pairing code"}
          </button>
        </div>
      </form>
    );
  }

  if (!pairingStatus.configured) {
    return (
      <p className="sec-muted">
        {pairingStatus.configurationError ?? "Pairing is not configured on the server yet. Set PAIRING_SECRET first."}
      </p>
    );
  }

  return (
    <div className="sec-pairing">
      <p className="sec-muted">
        Scan this from the KyPost app to pair a device. The app receives the server URL automatically.
        This code is good for {Math.round(ttlMs / 1000)} seconds and works once.
      </p>

      <div className="sec-pairing-scan">
        {qrDataUrl ? (
          <div className="sec-qr">
            <img
              src={qrDataUrl}
              alt="Native mobile pairing QR code"
              width={QR_CODE_WIDTH_PX}
              height={QR_CODE_WIDTH_PX}
            />
            {showBar ? (
              <div
                className="sec-qr-timer-track"
                style={{ width: `${QR_CODE_WIDTH_PX}px` }}
                aria-hidden="true"
              >
                <div
                  className="sec-qr-timer-bar"
                  style={{ width: `${barWidth}px`, background: pairingBarColor(remainingMs, ttlMs) }}
                />
              </div>
            ) : null}
          </div>
        ) : (
          <p className="sec-muted">Preparing pairing code…</p>
        )}

        <div className="sec-pairing-meta">
          <span className="sec-eyebrow">Subscriber ID</span>
          <strong>{pairingStatus.subscriberId || "Not available"}</strong>
        </div>

        <div className="sec-actions">
          <button type="button" className="sec-action-quiet" onClick={() => applyPairingStatus(null)}>
            New code
          </button>
          <button type="button" onClick={() => pairThisDevice(pairingStatus)}>
            Pair this device
          </button>
        </div>
      </div>
    </div>
  );
}
