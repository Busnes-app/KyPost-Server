package app

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Busnes-app/ky-primitives/recoveryclient"
	"github.com/Busnes-app/kypost-server/backend/internal/api"
	"github.com/Busnes-app/kypost-server/backend/internal/config"
	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

const setupLimit = 64 << 10

type setupBundle struct {
	Version       int              `json:"version"`
	SSO           *sso.SSOSettings `json:"sso,omitempty"`
	PairingSecret string           `json:"pairingSecret,omitempty"`
	Domains       []string         `json:"domains,omitempty"`
	Recovery      *setupRecovery   `json:"recovery,omitempty"`
}
type setupRecovery struct {
	URL  string `json:"url"`
	Code string `json:"code"`
}
type setupDNS struct {
	Domain      string `json:"domain"`
	Name        string `json:"name"`
	Value       string `json:"value"`
	Established bool   `json:"established"`
}
type setupReport struct {
	Version              int        `json:"version"`
	Applied              []string   `json:"applied"`
	Unchanged            []string   `json:"unchanged"`
	DNS                  []setupDNS `json:"dns"`
	Pending              []string   `json:"pending"`
	ConfigurationApplied bool       `json:"configurationApplied"`
}

func runApplySetup(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) != 2 || args[0] != "--file" || args[1] == "" {
		return errors.New("usage: kypost-server apply-setup --file <private-json-file|->")
	}
	if err := setupRoots(); err != nil {
		return err
	}
	var raw []byte
	var err error
	if args[1] == "-" {
		raw, err = io.ReadAll(io.LimitReader(stdin, setupLimit+1))
	} else {
		raw, _, err = setupExisting(args[1])
	}
	if err != nil || len(raw) > setupLimit {
		return errors.New("setup requires an owned regular private JSON file of at most 64 KiB, or protected stdin")
	}
	var b setupBundle
	if setupDecode(raw, &b) != nil {
		return errors.New("invalid setup JSON; use documented version-1 fields without trailing JSON")
	}
	if err = validateSetup(b); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, err := fsutil.LockFileContext(ctx, filepath.Join(config.ConfigDir(), "installer-setup"))
	if err != nil {
		return errors.New("setup busy; retry without deleting locks")
	}
	defer release()
	if err = sso.RequireNativeRestoreReleased(config.StateDir()); err != nil {
		return err
	}
	// Setup runs after base bootstrap, never creates or repairs instance storage.
	info, err := os.Lstat(filepath.Join(config.StateDir(), "state.db"))
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("existing instance state unavailable; start the base server before setup")
	}
	st, err := state.New(config.StateDir())
	if err != nil {
		return errors.New("existing instance state unavailable")
	}
	defer st.Close()
	svc, err := newBackupService(st)
	if err != nil {
		return errors.New("backup configuration invalid; correct deployment backup settings")
	}
	settings := sso.NewStore(config.ConfigDir()).Load()
	ssoPath := filepath.Join(config.ConfigDir(), "sso.json")
	_, ssoExists, err := setupExisting(ssoPath)
	if err != nil {
		return err
	}
	if b.SSO != nil {
		if ssoExists {
			data, _, e := setupExisting(ssoPath)
			var prior sso.SSOSettings
			if e != nil || setupDecode(data, &prior) != nil || prior != *b.SSO {
				return errors.New("existing SSO settings differ; setup preserves them, use protected Server SSO settings to change them")
			}
			settings = prior
		} else {
			settings = *b.SSO
		}
	}
	domains := sso.NewNativeDomainStore(config.ConfigDir())
	set, err := domains.ReadSet()
	if err != nil {
		return errors.New("domain state unavailable; preserve it and resolve migration before setup")
	}
	if len(b.Domains) > 0 && (!settings.Enabled || set.Issuer != "" && set.Issuer != settings.IssuerURL) {
		return errors.New("domain setup requires enabled SSO with the existing domain issuer")
	}
	for _, d := range b.Domains {
		if sso.ValidateNativeDomainSetup(d, settings.IssuerURL) != nil || slices.Contains(set.Retired, d) {
			return errors.New("setup requires valid lowercase non-retired domains; modify retired domains through Server Mail domain")
		}
	}
	if b.PairingSecret != "" {
		key, e := setupPairingSecret()
		if e != nil || key == "" {
			return errors.New("existing pairing authority unavailable; start the base server and resolve deployment key settings before setup")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(b.PairingSecret)) != 1 {
			return errors.New("pairing key differs; register the retained effective target key in KyIdentity, do not replace it")
		}
	}
	paired := false
	if b.Recovery != nil {
		bc, e := config.LoadBackupConfig()
		if e != nil || recoveryclient.ValidateURL(b.Recovery.URL, bc.AllowPrivateRecovery) != nil {
			return errors.New("recovery URL refused; require HTTPS and explicit private-recovery opt-in if needed")
		}
		status, e := svc.Status()
		if e != nil || status.KeyProblem != "" {
			return errors.New("recovery state unavailable; preserve its pin and repair before setup")
		}
		if status.Paired && status.KyRecoveryURL != b.Recovery.URL {
			return errors.New("existing recovery destination differs; setup preserves pairing")
		}
		paired = status.Paired
		if !paired {
			if b.Recovery.Code == "" {
				return errors.New("unpaired recovery requires a fresh six-digit code")
			}
			if _, exists, e := setupExisting(config.SecretFile("TOTP_SECRET_KEY_FILE", "totp-secret.key")); e != nil || !exists {
				return errors.New("start the base server once to initialize the backup sealing key")
			}
		}
	}
	report := setupReport{Version: 1, Applied: []string{}, Unchanged: []string{}, DNS: []setupDNS{}, Pending: []string{}}
	actor := "cli:" + strconv.Itoa(os.Geteuid())
	change := func(name string, needed bool, apply func() error) error {
		if !needed {
			report.Unchanged = append(report.Unchanged, name)
			return nil
		}
		if e := svc.Audit("installer.setup", actor, name, "started", nil); e != nil {
			return errors.New("setup audit unavailable; fix instance storage before retry")
		}
		e := apply()
		outcome := "committed"
		if e != nil {
			outcome = "failed"
		}
		if auditErr := svc.Audit("installer.setup", actor, name, outcome, nil); auditErr != nil {
			return errors.New("setup completion audit failed; inspect retained settings before retry")
		}
		if e != nil {
			return fmt.Errorf("setup %s failed; preserve applied settings and correct prerequisites before retry", name)
		}
		report.Applied = append(report.Applied, name)
		return nil
	}
	if b.PairingSecret != "" {
		report.Unchanged = append(report.Unchanged, "pairing-key")
	}
	if b.SSO != nil {
		data, _ := json.MarshalIndent(b.SSO, "", "  ")
		if err = change("sso", !ssoExists, func() error { return setupCreate(ctx, ssoPath, data) }); err != nil {
			return err
		}
	}
	for i, d := range b.Domains {
		_, exists := set.Domains[d]
		if err = change("domain-"+strconv.Itoa(i), !exists, func() error { _, e := domains.EnsureDomain(ctx, d, settings.IssuerURL); return e }); err != nil {
			return err
		}
	}
	if b.Recovery != nil {
		if err = change("recovery-pairing", !paired, func() error { _, e := svc.PairIfMissing(ctx, b.Recovery.URL, b.Recovery.Code); return e }); err != nil {
			return err
		}
	}
	set, err = domains.ReadSet()
	if err != nil {
		return errors.New("setup report unavailable; preserve configuration")
	}
	names := []string{}
	for name := range set.Domains {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		d := set.Domains[name]
		report.DNS = append(report.DNS, setupDNS{d.Domain, d.RecordName(), d.RecordValue(), d.Established})
		if !d.Established {
			report.Pending = append(report.Pending, "publish the reported TXT record and verify "+name+" in Server Mail domain")
		}
	}
	report.Pending = append(report.Pending, "verify HTTPS and forwarded client address through the intended edge", "configure and check the operator relay in Server Mail domain; confirm actual recipient receipt", "initialize a new spool once and configure the selected receiving profile; verify external receiver TLS and inbound delivery", "assign the separate everyday KyIdentity account and confirm mailbox access", "verify backup receipt and separate-host restore before production mail")
	report.ConfigurationApplied = true
	return json.NewEncoder(stdout).Encode(report)
}

func setupDecode(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func validateSetup(b setupBundle) error {
	if b.Version != 1 || b.SSO == nil && b.PairingSecret == "" && len(b.Domains) == 0 && b.Recovery == nil {
		return errors.New("setup requires version 1 and at least one documented setup section")
	}
	if b.SSO != nil && (!b.SSO.Enabled || b.SSO.AllowInsecureIssuer || !strings.HasPrefix(b.SSO.IssuerURL, "https://") || sso.ValidateIssuerURL(b.SSO.IssuerURL, false) != nil || strings.TrimSpace(b.SSO.ClientID) == "" || len(b.SSO.ClientID) > 512 || len(b.SSO.ClientSecret) > 4096 || strings.ContainsAny(b.SSO.ClientID+b.SSO.ClientSecret, "\r\n\x00")) {
		return errors.New("setup SSO requires enabled HTTPS, a client ID and bounded credentials")
	}
	if b.PairingSecret != "" && (len(b.PairingSecret) < 32 || len(b.PairingSecret) > 4096 || strings.TrimSpace(b.PairingSecret) != b.PairingSecret || strings.ContainsAny(b.PairingSecret, "\r\n\x00")) {
		return errors.New("pairingSecret must match the retained effective runtime secret, with 32 to 4096 characters")
	}
	if len(b.Domains) > 32 {
		return errors.New("setup accepts at most 32 domains")
	}
	if b.Recovery != nil && b.Recovery.Code != "" && (len(b.Recovery.Code) != 6 || strings.Trim(b.Recovery.Code, "0123456789") != "") {
		return errors.New("recovery code must be six digits")
	}
	return nil
}
func setupExisting(path string) ([]byte, bool, error) {
	info, e := os.Lstat(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil, false, nil
	}
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, false, errors.New("setup files must be owner-only regular files without symlinks")
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Geteuid() {
		return nil, false, errors.New("setup files must belong to the runtime user")
	}
	data, e := receivingTLSFile(path, true)
	return data, true, e
}
func setupCreate(ctx context.Context, path string, data []byte) error {
	release, e := fsutil.LockFileContext(ctx, path)
	if e != nil {
		return e
	}
	defer release()
	// No-replace publication also refuses a concurrent writer that does not use the lock.
	f, e := os.CreateTemp(filepath.Dir(path), ".setup-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, e = f.Write(data); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = os.Link(f.Name(), path); e != nil {
		return e
	}
	return fsutil.SyncDir(filepath.Dir(path))
}

func setupRoots() error {
	for _, p := range []string{config.ConfigDir(), config.StateDir(), config.SecretDir()} {
		info, e := os.Lstat(p)
		if e != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
			return errors.New("setup requires existing owner-only config, state and private directories")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(st.Uid) != os.Geteuid() {
			return errors.New("run apply-setup as the runtime user (--user kypost)")
		}
	}
	return nil
}

// Only the API originates this key. The installer verifies the effective secret,
// including the runtime's trimmed environment precedence, without replacing it.
func setupPairingSecret() (string, error) {
	path := config.SecretFile("PAIRING_SECRET_FILE", "pairing.key")
	if strings.TrimSpace(os.Getenv("PAIRING_SECRET")) == "" {
		if _, _, e := setupExisting(path); e != nil {
			return "", e
		}
	}
	return api.ReadPairingSecret(path), nil
}
