package app

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/mailmsg"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

type setupCheck struct {
	ID         string `json:"id"`
	Configured bool   `json:"configured"`
	Message    string `json:"message,omitempty"`
}
type setupStatus struct {
	Version                int          `json:"version"`
	ReadyForExternalChecks bool         `json:"readyForExternalChecks"`
	Checks                 []setupCheck `json:"checks"`
	ExternalChecks         []string     `json:"externalChecks"`
}

// Advisory configuration reads, not transport tests or a production readiness claim.
func runSetupStatus(args []string, out io.Writer) error {
	if len(args) != 0 {
		return errors.New("usage: kypost-server setup-status")
	}
	if e := setupRoots(); e != nil {
		return e
	}
	report := setupStatus{Version: 1, ReadyForExternalChecks: true, Checks: []setupCheck{}, ExternalChecks: []string{"public HTTPS/TLS and forwarded-client-address verification", "external receiver TLS/reachability and actual inbound mailbox delivery", "actual outbound receipt, authentication headers, attachments/PGP and Sent", "independent backup receipt and separate-host restore qualification"}}
	add := func(id string, ok bool, message string) {
		c := setupCheck{ID: id, Configured: ok}
		if !ok {
			c.Message = message
			report.ReadyForExternalChecks = false
		}
		report.Checks = append(report.Checks, c)
	}
	origin, originErr := url.Parse(os.Getenv("SERVER_BASE_URL"))
	add("public-origin", originErr == nil && origin.Scheme == "https" && origin.Hostname() != "" && origin.User == nil && (origin.Path == "" || origin.Path == "/") && origin.RawQuery == "" && origin.Fragment == "", "set SERVER_BASE_URL to the external HTTPS origin registered for the OIDC callback")
	var settings sso.SSOSettings
	data, exists, e := setupExisting(filepath.Join(config.ConfigDir(), "sso.json"))
	validSSO := e == nil && exists && setupDecode(data, &settings) == nil && settings.Enabled && !settings.AllowInsecureIssuer && strings.HasPrefix(settings.IssuerURL, "https://") && sso.ValidateIssuerURL(settings.IssuerURL, false) == nil && strings.TrimSpace(settings.ClientID) != ""
	add("identity", validSSO, "configure enabled HTTPS SSO with the installer bundle or Server SSO")
	key, e := setupPairingSecret()
	add("webhook-key", e == nil && key != "", "register the existing effective protected pairing secret with the matching KyIdentity webhook system")
	native, e := config.NativeMailEnabled()
	add("native-mail", e == nil && native, "set KYPOST_NATIVE_MAIL=true in the deployment and restart")
	domains, e := sso.NewNativeDomainStore(config.ConfigDir()).ReadSet()
	verified := e == nil && validSSO && domains.Issuer == settings.IssuerURL && len(domains.Domains) > 0
	for _, d := range domains.Domains {
		verified = verified && d.Established
	}
	add("mail-domains", verified, "publish and verify every configured domain TXT record in Server Mail domain; this status does not refresh DNS proof")
	add("restore-authority", sso.RequireNativeRestoreReleased(config.StateDir()) == nil, "keep the restore hold and follow the protected repair/release procedure")
	relay, exists, e := mailmsg.ReadDomainRelay(filepath.Join(config.ConfigDir(), "native-relay.json"), filepath.Join(config.SecretDir(), "native-relay.key"))
	add("relay", e == nil && exists && relay.Issuer == settings.IssuerURL, "configure the operator relay in Server Mail domain, then run Check saved relay")
	receiving, e := config.NativeReceivingEnabled()
	profile := os.Getenv("KYPOST_NATIVE_RECEIVER") == "true" || os.Getenv("KYPOST_CLOUDFLARE_RECEIVING_ORIGIN") != ""
	info, spoolErr := os.Lstat(filepath.Join(config.StateDir(), "receiving", "ingress.db"))
	add("receiving", e == nil && receiving && profile && spoolErr == nil && info.Mode().IsRegular(), "initialize a new spool once and select the bundled or hosted receiving profile; runtime and external delivery still require verification")
	bc, e := config.LoadBackupConfig()
	add("bulk-backup", e == nil && bc.BulkRepository != "", "configure KYPOST_BULK_BACKUP_REPOSITORY before mail exceeds ordinary capsule limits")
	info, e = os.Lstat(filepath.Join(config.StateDir(), "state.db"))
	if e != nil || !info.Mode().IsRegular() {
		return errors.New("instance state unavailable; start the base server before setup-status")
	}
	st, e := state.New(config.StateDir())
	if e != nil {
		return errors.New("instance state unavailable")
	}
	defer st.Close()
	svc, e := newBackupService(st)
	if e != nil {
		return errors.New("backup configuration invalid; correct deployment backup settings")
	}
	backup, e := svc.Status()
	if e != nil {
		return errors.New("backup status unavailable; preserve recovery state and resolve the backup error")
	}
	add("sealed-backup", backup.KeyID != "" && backup.KeyProblem == "" && (backup.Paired || backup.LocalDir != ""), "configure a recovery key and destination in Server Backup, or pair recovery with apply-setup")
	add("backup-schedule", backup.IntervalSec > 0, "enable the backup schedule in Server Backup")
	add("backup-receipt", backup.LastReceipt != nil || len(backup.LocalCopies) > 0, "run and inspect the first backup; a local copy alone does not prove an independent destination")
	latestOK := false
	for _, audit := range backup.Recent {
		if audit.Action == "admin.backup_run" || audit.Action == "admin.backup_local_fallback" || audit.Action == "admin.backup_intent" && (audit.Target == "run" || audit.Target == "deposit") {
			latestOK = audit.Action == "admin.backup_run" && audit.Outcome == "success"
			break
		}
	}
	add("backup-result", latestOK, "run and inspect a completed successful backup in Server Backup; the latest attempt failed, is incomplete, or has no recent success evidence")
	return json.NewEncoder(out).Encode(report)
}
