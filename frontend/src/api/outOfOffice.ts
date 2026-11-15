import { getJSON, putJSON } from "./client";

export type RemoteMode = "off" | "on" | "contacts";

/** Mirrors backend config.OutOfOffice (PUT /api/mail/out-of-office). Texts are plain text. */
export type OutOfOffice = {
  enabled: boolean;
  /** RFC 3339 instant, or "" for no start. */
  start: string;
  /** RFC 3339 instant, or "" for no end. */
  end: string;
  /** IANA zone the times were entered in. */
  timeZone: string;
  localMessage: string;
  remote: RemoteMode;
  remoteMessage: string;
};

/** GET /api/mail/out-of-office adds what the server decides. */
export type OutOfOfficeView = OutOfOffice & {
  /** On and inside its period right now. */
  active: boolean;
  /** The administrator lets replies go to outside senders. */
  remoteAllowed: boolean;
};

export const MAX_OUT_OF_OFFICE_TEXT = 4000;

export function getOutOfOffice(): Promise<OutOfOfficeView> {
  return getJSON<OutOfOfficeView>("/api/mail/out-of-office");
}

export function putOutOfOffice(o: OutOfOffice): Promise<OutOfOfficeView> {
  const { enabled, start, end, timeZone, localMessage, remote, remoteMessage } = o;
  return putJSON<OutOfOfficeView>("/api/mail/out-of-office", { enabled, start, end, timeZone, localMessage, remote, remoteMessage });
}
