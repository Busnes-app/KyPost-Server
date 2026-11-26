//go:build !kye2e

package config

// MultiTenantBuild is false in every release build. Only the suite end-to-end run builds with
// -tags kye2e (KyCalendar docs/superpowers/specs/2026-11-24-multi-tenant-e2e-design.md, Mechanism).
const MultiTenantBuild = false
