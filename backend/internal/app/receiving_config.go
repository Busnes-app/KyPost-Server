package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"
	"github.com/Busnes-app/kypost-server/backend/internal/sso"
)

func runReceivingConfig(args []string, output io.Writer) (result error) {
	defer func() {
		status := "generated"
		if result != nil {
			status = "refused"
		}
		slog.Info("receiving configuration", "actor", "operator", "task_id", "native-receiving", "action", "config", "target", "receiver-profile", "result", status, "correlation_id", "configuration")
	}()
	if len(args) != 4 {
		return errors.New("usage: receiving config <IP:port> <hostname> <certificate-file> <private-key-file>")
	}
	spamEnabled, err := receivingRspamdEnabled()
	if err != nil {
		return err
	}
	spamArgs := ""
	if spamEnabled {
		spamArgs = ` "{source_ip}" "{source_host}"`
	}
	address, err := netip.ParseAddrPort(args[0])
	if err != nil || address.Port() == 0 || address.Addr().Zone() != "" {
		return errors.New("receiving config requires an explicit literal IP and nonzero port")
	}
	host := args[1]
	if len(host) > 253 || !strings.Contains(host, ".") {
		return errors.New("receiving hostname must be a fully qualified ASCII DNS name")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("invalid receiving hostname")
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return errors.New("receiving hostname must use lowercase ASCII DNS labels")
			}
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := openReceivingRuntime(ctx, false)
	if err != nil {
		return err
	}
	defer r.holding.Close()
	return r.writeConfig(ctx, executable, address.String(), host, args[2], args[3], spamArgs, output)
}

// writeConfig renders the profile after the runtime's storage, TLS and fresh
// domain checks. Every configured domain is a Maddy destination.
func (r *receivingRuntime) writeConfig(ctx context.Context, executable, address, host, certPath, keyPath, spamArgs string, output io.Writer) error {
	for _, path := range []string{executable, r.stateDir, certPath, keyPath} {
		// Maddy expands {env:...} even inside quotes and only escapes quotes.
		if !filepath.IsAbs(path) || len(path) > 4096 || strings.ContainsAny(path, "\\\"{}$") || strings.ContainsFunc(path, unicode.IsControl) {
			return errors.New("receiving configuration paths must be absolute, without controls, quotes, backslashes or braces")
		}
	}
	certificate, err := receivingTLSFile(certPath, false)
	if err != nil {
		return err
	}
	key, err := receivingTLSFile(keyPath, true)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return errors.New("receiving certificate and private key must form a valid TLS pair")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname(host) != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return errors.New("receiving certificate must currently be valid for the explicit hostname")
	}
	serverUsage := len(leaf.ExtKeyUsage) == 0 && len(leaf.UnknownExtKeyUsage) == 0
	for _, usage := range leaf.ExtKeyUsage {
		serverUsage = serverUsage || usage == x509.ExtKeyUsageAny || usage == x509.ExtKeyUsageServerAuth
	}
	if !serverUsage {
		return errors.New("receiving certificate must permit TLS server authentication")
	}
	// Every configured domain is a destination; binds still refuse a domain
	// whose proof has lapsed, so one lapsed domain never blocks the others.
	domains, err := r.domains.ReadSet()
	if err != nil {
		return err
	}
	destinations := slices.Sorted(maps.Keys(domains.Domains))
	proofs := []sso.NativeDomain{}
	for _, domain := range destinations {
		if proof, err := r.domains.VerifyDomain(ctx, domain); err == nil {
			proofs = append(proofs, proof)
		}
	}
	release, err := fsutil.LockFileContext(ctx, filepath.Join(r.configDir, sso.NativeDomainsFile))
	if err != nil {
		return err
	}
	defer func() {
		if release != nil {
			release()
		}
	}()
	current, err := r.domains.ReadSet()
	settings := sso.NewStore(r.configDir).Load()
	if err != nil || len(proofs) == 0 || !slices.Equal(slices.Sorted(maps.Keys(current.Domains)), destinations) || !settings.Enabled || settings.IssuerURL != current.Issuer {
		return sso.ErrNativeDomain
	}
	for _, proof := range proofs {
		if !current.CurrentProof(proof) {
			return sso.ErrNativeDomain
		}
	}
	if err := sso.RequireNativeRestoreReleased(r.stateDir); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	config := fmt.Sprintf(`hostname %s
state_dir "%s"
runtime_dir "%s"
tls file "%s" "%s" {
    protocols tls1.2 tls1.3
}
log stderr
smtp tcp://%s {
    defer_sender_reject false
    max_message_size 4M
    max_header_size 128K
    max_recipients 100
    buffer ram
    read_timeout 30s
    write_timeout 30s
    shutdown_timeout 30s
    max_logged_rcpt_errors 0
    limits {
        all concurrency 4
        ip concurrency 2
        all rate 30 1m
        ip rate 10 1m
    }
    check {
        require_tls
        command "%s" receiving bind "{msg_id}" "{sender}" "{address}" {
            run_on rcpt
            code 1 reject 451 4.3.0 "Receiving storage unavailable"
            code 3 reject 550 5.1.1 "Recipient unavailable"
            code 6 reject 550 5.7.1 "Sender blocked"
            code 7 reject 550 5.1.7 "Sender address not accepted"
            code 8 reject 451 4.3.0 "Sender blocks unreadable"
        }
        command "%s" receiving accept "{msg_id}" "{sender}"%s {
            run_on body
            code 1 reject 451 4.3.0 "Receiving storage unavailable"
            code 3 reject 451 4.3.0 "Routing unavailable"
            code 4 reject 550 5.7.1 "Message rejected by spam policy"
            code 5 reject 451 4.7.0 "Spam check temporarily deferred; retry later"
        }
    }
    destination %s { deliver_to dummy }
    default_destination { reject }
}
`, host, r.stateDir, r.stateDir, certPath, keyPath, address, executable, executable, spamArgs, strings.Join(destinations, " "))
	// Output is a historical configuration, not continuing authority. Never hold
	// the domain fence across an operator's potentially blocked output pipe.
	release()
	release = nil
	_, err = io.WriteString(output, config)
	return err
}

func receivingTLSFile(path string, private bool) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("receiving TLS files must be existing regular files, without symlinks")
	}
	// A replacement between Lstat and open must not follow another private
	// file or block on a substituted FIFO. Read only the inspected inode.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("receiving TLS file unavailable; provide readable certificate and key files")
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Size() > 1<<20 || private && opened.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("receiving TLS files must be regular and at most 1 MiB; private key requires owner-only permissions")
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, errors.New("receiving TLS file could not be read within its size limit")
	}
	return data, nil
}
