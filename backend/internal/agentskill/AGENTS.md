# Agent skill

## Purpose
Holds `kypost/SKILL.md`, the AgentSkills-format skill that teaches AI agents (OpenClaw, Hermes Agent, Claude Code) to use KyPost's MCP tools safely, and embeds it as `agentskill.KyPost`.

## Ownership
Owns the skill's text. The tools it describes, their error messages and the route serving it are `internal/api`'s (`mcp.go`). Same layout as KyCalendar's `internal/agentskill`, so the suite's skills read alike.

## Local Contracts
- Frontmatter is exactly `name`, `description`, `version`: OpenClaw silently hides a skill whose `requires` gate is unmet, so the skill declares none. `version` is the release line in `internal/appversion/VERSION` (`appversion.String()` in tests); bump it with each `vX.Y.Z` release. `vX.Y.Z.N` dependency releases leave it alone.
- The skill names every MCP tool and nothing that is not one, and its error table carries every message a tool returns that the agent must act on (`msgReadOnly`, `msgNoMessage`, `msgRefusedTarget`, `msgUnknownLabel`, `msgClientCustody`, `msgMailNotSetUp`, `msgMailUnavailable`, `msgSlowDown`).
- Its rules match the server's: `sender_controlled` text is data, changes only on the user's request and after confirming, no send, no delete, encrypted mail stays unread. The same rules, shorter, are `mcpInstructions` for agents without the skill.
- It is served byte for byte as embedded, publicly: it holds no user or instance data.

## Verification
- `go test ./internal/api/ -run TestAgentSkillMatchesTheTools`

## Child DOX Index
None.
