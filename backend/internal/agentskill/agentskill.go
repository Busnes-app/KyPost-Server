// Package agentskill holds the SKILL.md that teaches AI agents (OpenClaw, Hermes Agent) to use
// KyPost's MCP tools. Each instance serves the copy matching its own tools.
package agentskill

import _ "embed"

// KyPost is kypost/SKILL.md, served at /agent/skills/kypost/SKILL.md.
//
//go:embed kypost/SKILL.md
var KyPost []byte
