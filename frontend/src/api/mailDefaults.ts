import { getJSON, putJSON } from "./client";

export type MailDefaults = { host: string; port: number; smtpHost: string; smtpPort: number };

export function getMailDefaults(): Promise<MailDefaults> {
  return getJSON<MailDefaults>("/api/mail-defaults");
}

export function putMailDefaults(d: MailDefaults): Promise<MailDefaults> {
  return putJSON<MailDefaults>("/api/mail-defaults", d);
}
