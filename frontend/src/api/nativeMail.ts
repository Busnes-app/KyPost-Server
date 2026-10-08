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
  if (new Set(domains.map(d => d.domain)).size !== domains.length) throw new Error("Duplicate mail domain; reload before making changes.");
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

export type MailAddress = { address: string; mailbox: string; kind: "primary" | "alias"; state: "active" | "disabled" | "reserved"; generation: number };
// usedBytes is null when the server could not read the mailbox; quotaBytes is 0 from an older server.
export type NativeMailbox = { mailbox: string; user: string; kind: "primary" | "extra"; state: "active" | "disabled"; prepared: boolean; addresses: MailAddress[]; usedBytes: number | null; quotaBytes: number };
// Quotas against the state filesystem; overcommitted is the server's 80% warning.
export type MailStorage = { quotaBytes: number; usedBytes: number; freeBytes: number; totalBytes: number; reserveBytes: number; overcommitted: boolean };
// Lowercase bare dot-atom, the only form the ledger stores.
const atom = "[a-z0-9!#$%&'*+/=?^_`{|}~-]+";
const addressPattern = new RegExp(`^${atom}(\\.${atom})*@[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`);
function readAddress(value: unknown): MailAddress {
  const data = object(value);
  const address = text(data.address), mailbox = text(data.mailbox), { kind, state, generation } = data;
  if (address.length > 254 || address.indexOf("@") > 64 || !addressPattern.test(address) || !mailbox ||
      (kind !== "primary" && kind !== "alias") || (state !== "active" && state !== "disabled" && state !== "reserved") ||
      integer(generation) < 1) {
    throw new Error("Invalid mail address record; reload before making changes.");
  }
  return { address, mailbox, kind, state, generation: integer(generation) };
}
function readMailbox(item: unknown): NativeMailbox {
  const entry = object(item);
  const mailbox = text(entry.mailbox), user = text(entry.user), { kind, state, prepared } = entry;
  if (!mailbox || (kind !== "primary" && kind !== "extra") || (state !== "active" && state !== "disabled") || typeof prepared !== "boolean" ||
      !Array.isArray(entry.addresses) || entry.addresses.length > 1000) throw new Error("Invalid mailbox list.");
  const addresses = entry.addresses.map(readAddress);
  if (addresses.some(a => a.mailbox !== mailbox)) throw new Error("Inconsistent mailbox list; reload before making changes.");
  const usedBytes = entry.usedBytes === undefined ? null : integer(entry.usedBytes);
  const quotaBytes = entry.quotaBytes === undefined ? 0 : integer(entry.quotaBytes);
  return { mailbox, user, kind, state, prepared, addresses, usedBytes, quotaBytes };
}
// Absent when the server could not measure its filesystem.
export function readMailStorage(value: unknown): MailStorage | null {
  const { storage } = object(value);
  if (storage === undefined) return null;
  const data = object(storage);
  if (typeof data.overcommitted !== "boolean") throw new Error("Invalid mailbox storage summary.");
  return { quotaBytes: integer(data.quotaBytes), usedBytes: integer(data.usedBytes), freeBytes: integer(data.freeBytes), totalBytes: integer(data.totalBytes), reserveBytes: integer(data.reserveBytes), overcommitted: data.overcommitted };
}
export function readMailAddresses(value: unknown): NativeMailbox[] {
  const data = object(value);
  if (!Array.isArray(data.mailboxes) || data.mailboxes.length > 10000) throw new Error("Invalid mailbox list.");
  const seen = new Set<string>();
  return data.mailboxes.map((item: unknown) => {
    const box = readMailbox(item);
    for (const a of box.addresses) {
      if (seen.has(a.address)) throw new Error("Inconsistent mailbox list; reload before making changes.");
      seen.add(a.address);
    }
    return box;
  });
}
// A warning means the change is committed but its receiving route is still pending.
function warningOf(value: unknown): string {
  const { warning } = object(value);
  return warning === undefined ? "" : text(warning);
}
// A change answer must name the address acted on.
export function readMailAddressChange(value: unknown, address: string): { record: MailAddress; warning: string } {
  const record = readAddress(value);
  if (record.address !== address.toLowerCase()) throw new Error("Address change answer does not match the request; reload before retrying.");
  return { record, warning: warningOf(value) };
}
export function readMailboxChange(value: unknown): { mailbox: NativeMailbox; warning: string } {
  return { mailbox: readMailbox(value), warning: warningOf(value) };
}

// currentMailbox/currentUser: today's owner of an unresolved delivery's
// address, not proven to be the original. Empty when the address is inactive.
export type QuarantinedRecipient = { address: string; mailbox: string; user: string; generation: number; currentMailbox: string; currentUser: string };
// unresolved: captured under a routing table this server does not know (for
// example after a restore); only a release to the current owner or a discard applies.
export type QuarantinedDelivery = { sequence: number; gateway: string; id: string; sender: string; receivedAt: string; size: number; recipients: QuarantinedRecipient[]; unresolved: boolean };
export type QuarantineResult = "released" | "discarded" | "partially_released";
const invalidQuarantine = "Invalid quarantine list; reload before releasing or discarding.";
// The server's envelope rules: identifiers up to 256, addresses up to 320, never CR, LF or NUL.
function envelope(value: unknown, max: number, empty = false): string {
  if (typeof value !== "string" || value.length > max || (!empty && !value) || /[\r\n\0]/.test(value)) throw new Error(invalidQuarantine);
  return value;
}
function readRecipient(value: unknown, unresolved: boolean): QuarantinedRecipient {
  const data = object(value);
  const optional = (v: unknown) => v === undefined ? "" : envelope(v, 1024, true);
  const r = { address: envelope(data.address, 320), mailbox: envelope(data.mailbox, 1024, unresolved), user: envelope(data.user, 1024, true), generation: integer(data.generation),
    currentMailbox: optional(data.currentMailbox), currentUser: optional(data.currentUser) };
  // Only an unresolved recipient has no frozen mailbox and may name a current owner.
  if (unresolved ? r.mailbox !== "" || r.user !== "" : r.currentMailbox !== "" || r.currentUser !== "") throw new Error(invalidQuarantine);
  return r;
}
// One page after `after`: at most 100, rising sequences, no repeated delivery.
export function readQuarantine(value: unknown, after: number): QuarantinedDelivery[] {
  const data = object(value);
  if (!Array.isArray(data.deliveries) || data.deliveries.length > 100) throw new Error(invalidQuarantine);
  let last = after;
  const seen = new Set<string>();
  return data.deliveries.map((item: unknown) => {
    const d = object(item);
    const sequence = integer(d.sequence), receivedAt = envelope(d.receivedAt, 64);
    const gateway = envelope(d.gateway, 256), id = envelope(d.id, 256), key = JSON.stringify([gateway, id]);
    if (sequence <= last || seen.has(key) || !Number.isFinite(Date.parse(receivedAt)) ||
        !Array.isArray(d.recipients) || d.recipients.length > 1000 || (d.unresolved !== undefined && typeof d.unresolved !== "boolean")) throw new Error(invalidQuarantine);
    last = sequence;
    seen.add(key);
    const unresolved = d.unresolved === true;
    return { sequence, gateway, id, sender: envelope(d.sender, 320, true), receivedAt, size: integer(d.size), recipients: d.recipients.map((r: unknown) => readRecipient(r, unresolved)), unresolved };
  });
}
// The answer must name the requested delivery and a result its action can produce.
export function readQuarantineChange(value: unknown, gateway: string, id: string, action: "release" | "discard"): QuarantineResult {
  const data = object(value);
  const allowed: readonly QuarantineResult[] = action === "release" ? ["released"] : ["discarded", "partially_released"];
  const result = allowed.find(r => r === data.result);
  if (result && data.gateway === gateway && data.id === id) return result;
  throw new Error("Quarantine answer does not match the request; reload before retrying.");
}

export type BlockKind = "address" | "domain";
export const blockReasons = ["spam", "phishing", "abuse", "other"] as const;
export type BlockReason = typeof blockReasons[number];
// until: Unix ms, or null for no expiry. Values are sender-chosen text.
export type SenderBlock = { id: string; kind: BlockKind; value: string; until: number | null; source: "manual" | "automatic"; level: number; createdAt: number; actor: string; reason: BlockReason };
export type BlockEvidence = { damaged: boolean; resetAt: number | null; domainBlocksFrom: number | null; goodFull: boolean; automaticFull: boolean };
const invalidBlocks = "Invalid sender block list; reload before changing blocks.";
const optionalTime = (v: unknown) => v === null ? null : integer(v);
function readSenderBlock(value: unknown): SenderBlock {
  const b = object(value);
  const { id, kind, value: v, source, level, actor, reason } = b;
  if (typeof id !== "string" || !/^[0-9a-f]{16}$/.test(id) || (kind !== "address" && kind !== "domain") ||
      typeof v !== "string" || !v || v.length > 320 || /[\r\n\0]/.test(v) || v.includes("@") !== (kind === "address") ||
      !(source === "manual" && level === 0 || source === "automatic" && Number.isSafeInteger(level) && Number(level) >= 1) ||
      typeof actor !== "string" || !actor || actor.length > 128 || !blockReasons.some(r => r === reason)) throw new Error(invalidBlocks);
  return { id, kind, value: v, until: optionalTime(b.until), source, level: Number(level), createdAt: integer(b.createdAt), actor, reason: reason as BlockReason };
}
export function readSenderBlocks(value: unknown): { blocks: SenderBlock[]; evidence: BlockEvidence } {
  const data = object(value), e = object(data.evidence);
  if (!Array.isArray(data.blocks) || data.blocks.length > 5000 || typeof e.damaged !== "boolean" || typeof e.goodFull !== "boolean" || typeof e.automaticFull !== "boolean") throw new Error(invalidBlocks);
  const blocks = data.blocks.map(readSenderBlock);
  if (new Set(blocks.map(b => b.id)).size !== blocks.length) throw new Error(invalidBlocks);
  return { blocks, evidence: { damaged: e.damaged, resetAt: optionalTime(e.resetAt), domainBlocksFrom: optionalTime(e.domainBlocksFrom), goodFull: e.goodFull, automaticFull: e.automaticFull } };
}
const blockMismatch = "Sender block answer does not match the request; reload before retrying.";
// The server lowercases A-Z only and stores a manual block as requested.
export function readSenderBlockAdded(value: unknown, want: { kind: BlockKind; value: string; until: number | null; reason: BlockReason }): SenderBlock {
  const b = readSenderBlock(object(value).block);
  if (b.kind !== want.kind || b.value !== want.value.replace(/[A-Z]/g, c => c.toLowerCase()) || b.until !== want.until || b.reason !== want.reason || b.source !== "manual") throw new Error(blockMismatch);
  return b;
}
export function readSenderBlockRemoved(value: unknown, id: string): string {
  const data = object(value);
  if (data.id !== id || data.result !== "unblocked") throw new Error(blockMismatch);
  return warningOf(value);
}
