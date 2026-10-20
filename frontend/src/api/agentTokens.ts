import { credentialFields, deriveCredential } from "./auth";
import { deleteJSON, getJSON, postJSON, putJSON } from "./client";
import { withSSOStepUp } from "./stepup";

/** What an AI agent token may do. The scope lives on the server, never in the token. */
export type AgentScope = "mcp:read" | "mcp:organize";

export type AgentToken = {
  id: string;
  label: string;
  scope: AgentScope;
  createdAt: string;
  lastUsedAt: string | null;
  /** Revoked automatically unless the token is used before then. */
  revokeAfter: string;
};

/** The caller's step-up: their account password, unused for a KySignOn session. */
export type AgentConfirm = { ssoSession: boolean; accountPassword: string };

export const AGENT_SCOPE_LABELS: Record<AgentScope, string> = {
  "mcp:read": "Read only",
  "mcp:organize": "Read and organize"
};

/** What a confirmation grants, by agent and access level; never a generic "an app wants access". */
export function agentGrant(label: string, scope: AgentScope): string {
  return `Give the AI agent “${label}” ${AGENT_SCOPE_LABELS[scope]} access to your mail.`;
}

/** Minting a token or changing one outlives the session, so the server re-proves the caller. */
async function confirmed<T>(confirm: AgentConfirm, purpose: string, body: object, send: (body: object, headers: Record<string, string>) => Promise<T>): Promise<T> {
  const signed = { ...body, ...(confirm.ssoSession ? {} : credentialFields(await deriveCredential("", confirm.accountPassword))) };
  return withSSOStepUp((headers) => send(signed, headers), purpose);
}

export async function listAgentTokens(): Promise<AgentToken[]> {
  return (await getJSON<{ tokens: AgentToken[] }>("/api/agent-tokens")).tokens;
}

/** Returns the token itself, which the server never shows again. */
export function createAgentToken(label: string, scope: AgentScope, confirm: AgentConfirm): Promise<{ token: string; agentToken: AgentToken }> {
  return confirmed(confirm, agentGrant(label, scope), { label, scope }, (b, h) => postJSON("/api/agent-tokens", b, h));
}

export function setAgentTokenScope(token: AgentToken, scope: AgentScope, confirm: AgentConfirm): Promise<AgentToken> {
  return confirmed(confirm, agentGrant(token.label, scope), { scope }, (b, h) => putJSON(`/api/agent-tokens/${encodeURIComponent(token.id)}`, b, h));
}

export function revokeAgentToken(id: string): Promise<void> {
  return deleteJSON(`/api/agent-tokens/${encodeURIComponent(id)}`);
}
