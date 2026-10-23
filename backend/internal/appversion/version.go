// Package appversion is the one answer to "which version is this?".
//
// VERSION holds the release line, X.Y.Z, and is the only file a human bumps.
// Automated dependency releases add a fourth number (X.Y.Z.N) without a commit:
// release-image.yml passes the release tag's version to the build, which sets
// release through -ldflags. Source and CI builds report the release line.
package appversion

import (
	_ "embed"
	"strings"
)

//go:embed VERSION
var line string

// release is set at image build time with
// -ldflags "-X github.com/Busnes-app/kypost-server/backend/internal/appversion.release=X.Y.Z[.N]".
var release string

// String is the version this binary reports: the left-hand side of the
// self-update check, /api/status, the MCP server and recovery manifests.
func String() string {
	if release != "" {
		return release
	}
	return strings.TrimSpace(line)
}
