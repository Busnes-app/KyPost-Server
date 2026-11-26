//go:build kye2e

package config

// MultiTenantBuild opens multi-tenant startup in the suite end-to-end build only; that build
// also refuses any issuer outside .test (CheckTestIssuer).
const MultiTenantBuild = true
