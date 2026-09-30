import { getJSON, postJSON, putJSON, deleteJSON } from "./client";

export type RuleScope = {
  folders?: string[];
};

export type MatchGroup = {
  op: string;
  conditions: Condition[];
};

export type Condition = {
  negate?: boolean;
  group?: MatchGroup;
  field?: string;
  /** Header name tested when field is "header", e.g. "X-Spam-Flag". */
  header?: string;
  comparator?: string;
  value?: string;
};

export type Action = {
  type: string;
  value?: string;
};

export type Rule = {
  id: string;
  name: string;
  enabled: boolean;
  order: number;
  scope: RuleScope;
  match: MatchGroup;
  actions: Action[];
  rev: number;
  createdAt: string;
  updatedAt: string;
  guiEditable: boolean;
};

type RulesListResponse = {
  rules: Rule[];
};

export async function listRules(): Promise<Rule[]> {
  const res = await getJSON<RulesListResponse>("/api/rules");
  return res.rules ?? [];
}

export function createRule(rule: Partial<Rule>): Promise<Rule> {
  return postJSON<Rule>("/api/rules", rule);
}

export function updateRule(id: string, rule: Partial<Rule>): Promise<Rule> {
  return putJSON<Rule>(`/api/rules/${encodeURIComponent(id)}`, rule);
}

export function deleteRule(id: string): Promise<{ ok: boolean; removed: boolean }> {
  return deleteJSON<{ ok: boolean; removed: boolean }>(`/api/rules/${encodeURIComponent(id)}`);
}

export function reorderRules(ids: string[]): Promise<{ ok: boolean }> {
  return postJSON<{ ok: boolean }>("/api/rules/reorder", { ids });
}

export async function getRuleSieve(id: string): Promise<string> {
  const res = await getJSON<{ script: string }>(`/api/rules/${encodeURIComponent(id)}/sieve`);
  return res.script;
}

export function putRuleSieve(id: string, script: string): Promise<Rule> {
  return putJSON<Rule>(`/api/rules/${encodeURIComponent(id)}/sieve`, { script });
}

export type RunRulesResult = {
  scanned: number;
  matched: number;
  applied: number;
  failed: number;
};

export function runRulesNow(mailbox: string): Promise<RunRulesResult> {
  return postJSON<RunRulesResult>("/api/rules/run", { mailbox });
}
