import { getJSON } from "./client";

export type MailDomain =
  | { kind: "unconfigured" }
  | {
      kind: "configured";
      domain: string;
      issuer: string;
      recordName: string;
      recordValue: string;
      established: boolean;
      expiresAt: number;
      verifiedUntil: number;
    };
export type MailRelay =
  | { kind: "unconfigured" }
  | { kind: "configured"; domain: string; issuer: string; host: string; port: number; sendingEnabled: boolean };

function object(value: unknown): Record<string, unknown> {
  if (typeof value !== "object" || value === null || Array.isArray(value)) {
    throw new Error("Invalid mail setup response; reload before making changes.");
  }
  return Object.fromEntries(Object.entries(value));
}
function text(value: unknown): string {
  if (typeof value !== "string" || value.length > 1024) throw new Error("Invalid mail setup text.");
  return value;
}
function integer(value: unknown): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) throw new Error("Invalid mail setup number.");
  return value;
}
export function readMailDomain(value: unknown): MailDomain {
  const data = object(value);
  if (data.receivingEnabled !== false) throw new Error("Unsupported receiving setup; reload with a compatible client.");
  if (data.configured === false) {
    const claim = object(data.claim);
    if (claim.domain !== "" || claim.issuer !== "" || claim.token !== "" || data.recordName !== "" || data.recordValue !== "") throw new Error("Inconsistent mail domain status.");
    return { kind: "unconfigured" };
  }
  if (data.configured !== true) throw new Error("Invalid mail domain status.");
  const claim = object(data.claim);
  const domain = text(claim.domain);
  const token = text(claim.token);
  const issuer = text(claim.issuer);
  const recordName = text(data.recordName);
  const recordValue = text(data.recordValue);
  if (!domain || !issuer || !/^[a-f0-9]{64}$/.test(token) ||
      recordName !== `_kypost-mail.${domain}` || recordValue !== `kypost-mail-verify=${token}` ||
      (claim.established !== undefined && typeof claim.established !== "boolean")) {
    throw new Error("Invalid mail domain proof; reload before making changes.");
  }
  return { kind: "configured", domain, issuer, recordName, recordValue,
    established: claim.established === true, expiresAt: integer(claim.expiresAt),
    verifiedUntil: integer(claim.verifiedUntil ?? 0) };
}
export function readMailRelay(value: unknown): MailRelay {
  const data = object(value);
  if (data.transport !== "implicit-tls" || data.authRequired !== true || typeof data.sendingEnabled !== "boolean") {
    throw new Error("Unsupported relay transport; reload with a compatible client.");
  }
  if (data.configured === false && data.sendingEnabled === false) return { kind: "unconfigured" };
  if (data.configured !== true) throw new Error("Invalid mail relay status.");
  const port = integer(data.port);
  const host = text(data.host), domain = text(data.domain), issuer = text(data.issuer);
  if (!host || !domain || !issuer || !text(data.generation) || port < 1 || port > 65535) throw new Error("Invalid mail relay profile.");
  return { kind: "configured", domain, issuer, host, port, sendingEnabled: data.sendingEnabled };
}
export async function loadNativeMail() {
  const [domain, relay] = await Promise.all([
    getJSON<unknown>("/api/admin/mail-domain"),
    getJSON<unknown>("/api/admin/mail-relay"),
  ]);
  const claim = readMailDomain(domain), profile = readMailRelay(relay);
  if (profile.kind === "configured" && (claim.kind !== "configured" || profile.domain !== claim.domain || profile.issuer !== claim.issuer)) {
    throw new Error("Mail domain and relay ownership differ; preserve configuration and restore the matching profile.");
  }
  return { domain: claim, relay: profile };
}
