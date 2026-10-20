import { useCallback, useEffect, useState } from "react";
import { toErrorMessage } from "../../../api/client";
import {
  AGENT_SCOPE_LABELS as SCOPE_LABELS,
  agentGrant,
  createAgentToken,
  listAgentTokens,
  revokeAgentToken,
  setAgentTokenScope,
  type AgentScope,
  type AgentToken
} from "../../../api/agentTokens";
import { useAuth } from "../../../auth";
import { holdForSecret, releaseSecretHold } from "../../../lib/secretHold";

const ORGANIZE_WARNING =
  "The agent can label, move and archive your mail and save drafts. Anyone who can email you or message the agent can try to make it do so.";

/** The agent configs, reading the token from KYPOST_TOKEN so it is never pasted into a file. */
export function agentConfigs(origin: string): { name: string; text: string }[] {
  const url = `${origin}/mcp`;
  return [
    {
      name: "OpenClaw",
      text: JSON.stringify(
        { mcp: { servers: { kypost: { url, transport: "streamable-http", headers: { Authorization: "Bearer ${KYPOST_TOKEN}" } } } } },
        null,
        2
      )
    },
    {
      name: "Hermes Agent",
      text: `mcp_servers:\n  kypost:\n    url: "${url}"\n    headers:\n      Authorization: "Bearer \${KYPOST_TOKEN}"`
    },
    {
      name: "Claude Code (.mcp.json)",
      text: JSON.stringify(
        { mcpServers: { kypost: { type: "http", url, headers: { Authorization: "Bearer ${KYPOST_TOKEN}" } } } },
        null,
        2
      )
    },
    {
      name: "Skill",
      text:
        `# Hermes Agent\nhermes skills install ${origin}/agent/skills/kypost/SKILL.md --category productivity\n\n` +
        `# OpenClaw (or <workspace>/skills when OPENCLAW_STATE_DIR is set)\nmkdir -p ~/.agents/skills/kypost\n` +
        `curl -fsSo ~/.agents/skills/kypost/SKILL.md ${origin}/agent/skills/kypost/SKILL.md`
    }
  ];
}

function day(iso: string): string {
  return new Date(iso).toLocaleDateString();
}

type AgentAccessProps = {
  // Lets SecurityPage block tab switches while a new token is on screen.
  onRevealedTokenChange?: (revealed: boolean) => void;
};

export function AgentAccess({ onRevealedTokenChange }: AgentAccessProps = {}) {
  const { ssoSession = false } = useAuth();
  const [tokens, setTokens] = useState<AgentToken[] | null>(null);
  const [label, setLabel] = useState("");
  const [scope, setScope] = useState<AgentScope>("mcp:read");
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState("");
  const [revealed, setRevealed] = useState("");
  // Who the revealed token is for, and what it may do; named beside the token.
  const [revealedGrant, setRevealedGrant] = useState("");

  const refresh = useCallback(async () => {
    try {
      setTokens(await listAgentTokens());
    } catch (error: unknown) {
      setMessage(`Could not list AI agent tokens: ${toErrorMessage(error, "unknown error")}`);
    }
  }, []);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  useEffect(() => {
    onRevealedTokenChange?.(revealed !== "");
    if (revealed) {
      holdForSecret("Copy the new AI agent token and choose “Done — I saved it” before leaving — it will not be shown again.");
    } else {
      releaseSecretHold();
    }
    return () => {
      onRevealedTokenChange?.(false);
      releaseSecretHold();
    };
  }, [revealed, onRevealedTokenChange]);

  const confirm = { ssoSession, accountPassword: password };
  const locked = busy || (!ssoSession && !password);

  async function run(action: () => Promise<void>) {
    setBusy(true);
    setMessage("");
    try {
      await action();
    } catch (error: unknown) {
      setMessage(toErrorMessage(error, "The request failed"));
    } finally {
      // The password confirms one change, never a session of them.
      setPassword("");
      setBusy(false);
    }
  }

  function create() {
    void run(async () => {
      const made = await createAgentToken(label.trim(), scope, confirm);
      setRevealed(made.token);
      setRevealedGrant(`“${made.agentToken.label}” — ${SCOPE_LABELS[made.agentToken.scope]}`);
      setLabel("");
      setScope("mcp:read");
      await refresh();
    });
  }

  function changeScope(token: AgentToken, next: AgentScope) {
    const warning = next === "mcp:organize" ? ` ${ORGANIZE_WARNING}` : "";
    if (!window.confirm(`${agentGrant(token.label, next)}${warning}`)) {
      return;
    }
    void run(async () => {
      await setAgentTokenScope(token, next, confirm);
      await refresh();
    });
  }

  function revoke(token: AgentToken) {
    if (!window.confirm(`Revoke “${token.label}”? The agent loses access to your mail at once.`)) {
      return;
    }
    void run(async () => {
      await revokeAgentToken(token.id);
      await refresh();
    });
  }

  return (
    <div className="config-card">
      <h3>AI agents</h3>
      <p className="config-muted">
        AI agents such as OpenClaw, Hermes Agent and Claude Code can work with your mail over MCP at{" "}
        <code>{window.location.origin}/mcp</code>. An agent never sees end-to-end encrypted mail, and no agent can
        send or delete mail: it saves drafts that you send yourself. A token unused for 90 days is revoked.
      </p>
      <p className="notice notice-warning" role="note">
        Mail is written by other people, and an agent may treat it as instructions. Give an agent “Read and
        organize” only if you trust everyone who can email you or message the agent.
      </p>

      {revealed ? (
        <div className="contacts-dav-reveal">
          <p>
            <strong>New token for {revealedGrant}</strong>
          </p>
          <p className="config-muted">
            Copy this token now — it will not be shown again. Store it as <code>KYPOST_TOKEN</code> in the agent&apos;s
            environment; the configs below read it from there.
          </p>
          <div className="contacts-dav-secret">
            <code>{revealed}</code>
            <button type="button" onClick={() => void navigator.clipboard?.writeText(revealed)}>
              Copy
            </button>
            <button type="button" onClick={() => setRevealed("")}>
              Done — I saved it
            </button>
          </div>
          {agentConfigs(window.location.origin).map((c) => (
            <div key={c.name}>
              <p className="config-muted">{c.name}</p>
              <pre>
                <code>{c.text}</code>
              </pre>
            </div>
          ))}
        </div>
      ) : null}

      <h4>New AI agent token</h4>
      <label className="sec-label">
        <span>Name</span>
        <input className="sec-input" value={label} maxLength={64} disabled={busy} onChange={(e) => setLabel(e.target.value)} />
      </label>
      {(Object.keys(SCOPE_LABELS) as AgentScope[]).map((s) => (
        <label key={s} className="config-checkbox">
          <input type="radio" name="agent-scope" checked={scope === s} disabled={busy} onChange={() => setScope(s)} />
          <span>{s === "mcp:read" ? "Read only: search and read mail, list folders and labels" : "Read and organize: also label, move, archive and save drafts"}</span>
        </label>
      ))}
      {scope === "mcp:organize" ? <p className="notice notice-warning">{ORGANIZE_WARNING}</p> : null}
      {ssoSession ? (
        <p className="sec-muted">Creating a token and changing its access are confirmed with KySignOn.</p>
      ) : (
        <label className="sec-label">
          <span>Account password to create a token or change its access</span>
          <input
            className="sec-input"
            type="password"
            autoComplete="current-password"
            value={password}
            disabled={busy}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>
      )}
      <div className="config-actions">
        <button type="button" onClick={create} disabled={locked || label.trim() === ""}>
          {busy ? "Working..." : label.trim() ? `Create token for “${label.trim()}” (${SCOPE_LABELS[scope]})` : "Create token"}
        </button>
      </div>
      {message ? (
        <p className="notice notice-error" role="alert">
          {message}
        </p>
      ) : null}

      <h4>Your AI agent tokens</h4>
      {tokens === null ? (
        <p className="config-muted">Loading…</p>
      ) : tokens.length === 0 ? (
        <p className="config-muted">No AI agent tokens yet.</p>
      ) : (
        <ul className="sec-list">
          {tokens.map((t) => (
            <li key={t.id}>
              <strong>{t.label}</strong>{" "}
              <span className="config-muted">
                {t.lastUsedAt ? `last used ${day(t.lastUsedAt)}` : "never used"} · revoked if unused by {day(t.revokeAfter)}
              </span>
              <div className="config-actions">
                <select
                  aria-label={`Access for ${t.label}`}
                  value={t.scope}
                  disabled={locked}
                  onChange={(e) => changeScope(t, e.target.value as AgentScope)}
                >
                  {(Object.keys(SCOPE_LABELS) as AgentScope[]).map((s) => (
                    <option key={s} value={s}>
                      {SCOPE_LABELS[s]}
                    </option>
                  ))}
                </select>
                <button type="button" onClick={() => revoke(t)} disabled={busy}>
                  Revoke
                </button>
              </div>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
