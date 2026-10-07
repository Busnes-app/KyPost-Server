import { getJSON, HttpError } from "./client";

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
  | { kind: "configured"; domain: string; domains: string[]; retiredDomains: string[]; issuer: string; host: string; port: number; generation: string; sendingEnabled: boolean };
export type MailDomainEntry = { domain: string; retired: boolean; recordName: string; recordValue: string; established: boolean; expiresAt: number; verifiedUntil: number };
export type MailDomainSet = { issuer: string; founding: string; domains: MailDomainEntry[] };

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
function texts(value: unknown): string[] {
  if (!Array.isArray(value) || value.length > 1000) throw new Error("Invalid mail setup list.");
  return value.map(text);
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
  const generation = text(data.generation);
  if (!host || !domain || !issuer || !generation || port < 1 || port > 65535) throw new Error("Invalid mail relay profile.");
  // Servers before the domain set report only the one relay domain.
  const domains = data.domains === undefined ? [domain] : texts(data.domains);
  const retiredDomains = data.retiredDomains === undefined ? [] : texts(data.retiredDomains);
  if (!domains.includes(domain)) throw new Error("Invalid mail relay profile.");
  return { kind: "configured", domain, domains, retiredDomains, issuer, host, port, generation, sendingEnabled: data.sendingEnabled };
}
export function readMailDomains(value: unknown): MailDomainSet {
  const data = object(value);
  if (data.receivingEnabled !== false) throw new Error("Unsupported receiving setup; reload with a compatible client.");
  const issuer = text(data.issuer), founding = text(data.founding);
  if (!Array.isArray(data.domains) || data.domains.length > 1000) throw new Error("Invalid mail domain list.");
  const domains = data.domains.map((item: unknown) => {
    const entry = object(item);
    const domain = text(entry.domain), recordName = text(entry.recordName), recordValue = text(entry.recordValue);
    const { retired, established } = entry;
    if (!domain || typeof retired !== "boolean" || entry.configured !== !retired || typeof established !== "boolean" ||
        (retired ? recordName !== "" || recordValue !== "" || established
          : recordName !== `_kypost-mail.${domain}` || !/^kypost-mail-verify=[a-f0-9]{64}$/.test(recordValue))) {
      throw new Error("Invalid mail domain proof; reload before making changes.");
    }
    return { domain, retired, recordName, recordValue, established, expiresAt: integer(entry.expiresAt), verifiedUntil: integer(entry.verifiedUntil) };
  });
  const inService = domains.filter(d => !d.retired);
  if ((founding !== "") !== (inService.length > 0) || founding && !inService.some(d => d.domain === founding) || inService.length > 0 && !issuer) {
    throw new Error("Inconsistent mail domain status.");
  }
  return { issuer, founding, domains };
}
const ownershipDiffers = "Mail domain and relay ownership differ; preserve configuration and restore the matching profile.";
export type NativeMailSetup =
  | { mode: "set"; domains: MailDomainSet; relay: MailRelay }
  | { mode: "single"; domain: MailDomain; relay: MailRelay };
export async function loadNativeMail(): Promise<NativeMailSetup> {
  const [set, relay] = await Promise.all([
    // A server without the domain-set API answers 404; use the single-domain view.
    getJSON<unknown>("/api/admin/mail-domains").catch((e: unknown) => {
      if (e instanceof HttpError && e.status === 404) return null;
      throw e;
    }),
    getJSON<unknown>("/api/admin/mail-relay"),
  ]);
  const profile = readMailRelay(relay);
  if (set !== null) {
    const domains = readMailDomains(set);
    const live = new Set(domains.domains.filter(d => !d.retired).map(d => d.domain));
    if (profile.kind === "configured" && (profile.issuer !== domains.issuer || !profile.domains.every(d => live.has(d)))) throw new Error(ownershipDiffers);
    return { mode: "set", domains, relay: profile };
  }
  const claim = readMailDomain(await getJSON<unknown>("/api/admin/mail-domain"));
  if (profile.kind === "configured" && (claim.kind !== "configured" || profile.domain !== claim.domain || profile.issuer !== claim.issuer)) {
    throw new Error(ownershipDiffers);
  }
  return { mode: "single", domain: claim, relay: profile };
}

export function readMailRelayCheck(value: unknown, relay: MailRelay): void {
  const data = object(value);
  if (relay.kind !== "configured" || data.generation !== relay.generation ||
      data.host !== relay.host || data.port !== relay.port || data.tls !== true ||
      data.authenticated !== true || data.deliveryTested !== false) {
    throw new Error("Relay check did not match the saved profile. Reload before retrying.");
  }
}
