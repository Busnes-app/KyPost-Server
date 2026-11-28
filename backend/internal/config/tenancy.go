package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
)

// Tenancy modes (KYPOST_TENANCY). Unset is single.
const (
	TenancySingle = "single"
	TenancyMulti  = "multi"
)

// orgIDPattern is KyIdentity's org_id: an opaque UUID in its canonical lowercase form, so
// the string comparison K7 makes cannot disagree on case.
var orgIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidOrgID reports whether id is a canonical org_id.
func ValidOrgID(id string) bool { return orgIDPattern.MatchString(id) }

// MaxCells is KyIdentity's cap on an app's region map (KyIdentity tenancy spec, Cells).
const MaxCells = 32

// cellSlug is KyIdentity's home region slug (Cells, C1).
var cellSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)

// Cells parses KY_CELLS, the KyIdentity home regions this deployment serves (Cells, C5):
// 1 to MaxCells slugs, comma-separated, none repeated, returned sorted.
func Cells(raw string) ([]string, error) {
	var cells []string
	for c := range strings.SplitSeq(raw, ",") {
		if c = strings.TrimSpace(c); c == "" {
			continue
		}
		if !cellSlug.MatchString(c) || slices.Contains(cells, c) {
			return nil, errors.New("KY_CELLS lists a value that is not a KyIdentity home region slug, or one twice")
		}
		cells = append(cells, c)
	}
	if len(cells) == 0 || len(cells) > MaxCells {
		return nil, errors.New("KYPOST_TENANCY=multi requires KY_CELLS: 1 to 32 KyIdentity home regions this deployment serves, comma-separated")
	}
	slices.Sort(cells)
	return cells, nil
}

// Tenancy reads KYPOST_TENANCY, KYPOST_STAFF_ORG_ID and KY_CELLS. Multi mode requires a
// valid staff org and cells; single mode refuses both, so a half-configured multi install
// never starts single. Values are never echoed: the staff org is an identifier we do not log.
func Tenancy() (mode, staffOrg string, err error) {
	staffOrg = strings.TrimSpace(os.Getenv("KYPOST_STAFF_ORG_ID"))
	cells := strings.TrimSpace(os.Getenv("KY_CELLS"))
	switch strings.TrimSpace(os.Getenv("KYPOST_TENANCY")) {
	case "", TenancySingle:
		if staffOrg != "" {
			return "", "", errors.New("KYPOST_STAFF_ORG_ID is set, but KYPOST_TENANCY is not multi: unset it for a self-hosted install")
		}
		if cells != "" {
			return "", "", errors.New("KY_CELLS is set, but KYPOST_TENANCY is not multi: unset it for a self-hosted install")
		}
		if s3Set() {
			return "", "", errors.New("KYPOST_S3_* is set, but KYPOST_TENANCY is not multi: a self-hosted install keeps message bodies in its database")
		}
		return TenancySingle, "", nil
	case TenancyMulti:
		if !ValidOrgID(staffOrg) {
			return "", "", errors.New("KYPOST_TENANCY=multi requires KYPOST_STAFF_ORG_ID, the staff org's id as a lowercase UUID")
		}
		if _, err := Cells(cells); err != nil {
			return "", "", err
		}
		return TenancyMulti, staffOrg, nil
	default:
		return "", "", errors.New("KYPOST_TENANCY must be single or multi")
	}
}

// ErrMultiTenancyUnavailable refuses KYPOST_TENANCY=multi until hosted mail is built
// (docs/superpowers/specs/2026-11-20-tenancy-design.md, build plan PR 7). Only the build-tagged
// constant MultiTenantBuild lifts it; a constant, so no environment value or -ldflags -X can.
var ErrMultiTenancyUnavailable = errors.New("KYPOST_TENANCY=multi is not available in this release; unset it or set it to single")

// CheckTestIssuer refuses an issuer outside the reserved .test domain (RFC 2606). The
// end-to-end test build applies it to every issuer it saves or loads, so a test image
// deployed by mistake cannot take tokens from a real issuer.
func CheckTestIssuer(issuer string) error {
	u, err := url.Parse(issuer)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".test") {
		return fmt.Errorf("this end-to-end test build serves only an https issuer under .test, got %q", issuer)
	}
	return nil
}
