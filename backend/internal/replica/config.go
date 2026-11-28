// Package replica holds the PostgreSQL replica startup checks: one volume set, one host, no
// single-consumer receiver, and one shared configuration.
package replica

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"

	"github.com/Busnes-app/kypost-server/backend/internal/cryptutil"
	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// FingerprintSetting is the global meta key holding the shared-configuration record. It is
// never served or logged, and a PostgreSQL restore deletes it.
const FingerprintSetting = "replica_config_fingerprint"

// KeyField names the check value of totp-secret.key, the fingerprint's own key. Every other
// value is keyed by it, so a differing key is reported alone.
const KeyField = "totp-secret.key"

// Shared settings must be equal on every PostgreSQL replica: they decide who may sign in,
// which addresses are trusted, what mail is accepted and where backups go. Each value is
// compared as canonical; secret-file path variables compare the path (the shared volume makes
// the bytes equal).
var Shared = map[string]func(string) string{
	"SERVER_BASE_URL": trim, "PAIRING_SECRET": trim, "PAIRING_SECRET_FILE": trim,
	"POW_SECRET": trim, "POW_SECRET_FILE": trim, "POW_MAX_NUMBER": trim,
	"CAPTCHA_PROVIDER": lower, "CAPTCHA_SITE_KEY": trim, "CAPTCHA_SECRET_KEY": trim,
	"TRUSTED_PROXY_CIDRS": networks, "TRUSTED_PROXY_HEADER": lower, "RATE_LIMIT_EXEMPT_CIDRS": networks,
	"SANDBOX_PRIVATE_HOSTS": list, "ALLOW_INSECURE_SMTP": lower, "ALLOW_INSECURE_HTTP": lower,
	"KYPOST_NATIVE_MAIL": lower, "KYPOST_NATIVE_RECEIVING": lower, "KYPOST_MAILBOX_QUOTA_BYTES": trim,
	"KYPOST_RECEIVING_RSPAMD": lower, "KYPOST_CLOUDFLARE_RECEIVING_ORIGIN": trim, "KYPOST_NATIVE_RESTORE_RELEASE": lower,
	"KYPOST_KYCALENDAR_URL": trim, "KYPOST_KYCALENDAR_SECRET_FILE": trim, "KYPOST_KYCALENDAR_ALLOW_PRIVATE": lower,
	"KYPOST_KYDRIVE_URL": trim, "KYPOST_KYDRIVE_SECRET_FILE": trim, "KYPOST_KYDRIVE_ALLOW_PRIVATE": lower,
	"KYPOST_KYMESSAGE_URL": trim, "KYPOST_KYMESSAGE_SECRET_FILE": trim, "KYPOST_KYMESSAGE_ALLOW_PRIVATE": lower,
	"PUSH_RELAY_URL": trim, "PUSH_RELAY_KEY": trim, "PUSH_RELAY_KEY_FILE": trim,
	"APNS_RELAY_URL": trim, "APNS_RELAY_KEY": trim, "APNS_RELAY_KEY_FILE": trim,
	"KYPOST_TLS_CERT_FILE": trim, "KYPOST_TLS_PIN_PROBE": lower, "TLS_CERT_FILE": trim, "TLS_KEY_FILE": trim,
	"KYPOST_DISABLE_AI_SORTING": lower, "CLASSIFIER_ENGINE": lower, "OLLAMA_MODEL": trim, "TUNING_FILE": trim,
	"KYPOST_BACKUP_DIR": trim, "KYPOST_BACKUP_KEEP": trim, "KYPOST_BACKUP_DEPOSIT_INTERVAL": trim,
	"KYPOST_BACKUP_ALLOW_PRIVATE_RECOVERY": lower, "KYPOST_BULK_BACKUP_REPOSITORY": trim, "REVIEW_PAIRING_USERNAME": trim,
	"IMAP_CONFIG_FILE": trim, "IMAP_CONFIG_KEY_FILE": trim, "TOTP_SECRET_KEY_FILE": trim, "PGP_PRIVATE_KEY_FILE": trim,
	"PICKUP_STORE_KEY_FILE": trim, "RELAY_IDENTITY_KEY_FILE": trim,
	"CONFIG_DIR": trim, "SECRET_DIR": trim, "STATE_DIR": trim,
}

// MultiShared are shared settings only multi mode sets (KY_CELLS, the cells this deployment
// serves). Each joins the fingerprint only when set, so a single-tenant record never changes.
var MultiShared = map[string]func(string) string{"KY_CELLS": list,
	// MH11 object storage: every replica of a cell writes one bucket with one identity.
	"KYPOST_S3_ENDPOINT": trim, "KYPOST_S3_BUCKET": trim, "KYPOST_S3_REGION": trim, "KYPOST_S3_ACCESS_KEY": trim,
	"KYPOST_S3_SECRET_KEY": trim, "KYPOST_S3_SECRET_KEY_FILE": trim}

// PerHost settings may differ between replicas: listeners, logging, resources, the local
// model service, receiving concurrency, scratch and tools, the database URL itself, test and
// Compose-only variables, and the receiver knobs the replica profile refuses anyway.
var PerHost = []string{
	"WEB_PORT", "TZ", "KY_LOG_LEVEL", "LOG_DIR", "GODEBUG", "PATH", "CI", "MFA_CLEAR_ALL", "UNHEALTHY_RESTART_SECONDS",
	"KYPOST_MEM_LIMIT", "KYPOST_CPUS", "KYPOST_BIND", "KYPOST_IMAGE", "KYPOST_VERSION", "KYPOST_IP", "KYPOST_NETWORK",
	"KYPOST_SUBNET", "KYPOST_DNS", "ENV_FILE", "FRONTEND_DIR", "INSTALL_OLLAMA", "OLLAMA_MODELS_HOST_DIR",
	"OLLAMA_BASE_URL", "OLLAMA_API_KEY", "OLLAMA_GENERATE_PATH", "CLASSIFIER_BASE_URL", "CLASSIFY_CONCURRENCY",
	"CLASSIFY_PACE_MS", "EMBED_MODEL_DIR", "EMBED_MIN_CONFIDENCE",
	"KYPOST_RECEIVING_CONCURRENCY", "KYPOST_RECEIVING_IP_CONCURRENCY", "KYPOST_RECEIVING_RATE_PER_MINUTE",
	"KYPOST_RECEIVING_IP_RATE_PER_MINUTE", "KYPOST_BACKUP_SCRATCH_DIR", "KYPOST_RESTIC_BINARY",
	"KYPOST_DATABASE_URL", "KYPOST_DATABASE_URL_FILE", "PGSSLMODE", "BOOTSTRAP_ADMIN_USER", "BOOTSTRAP_ADMIN_PASS",
	"SMTP_HOST", "SMTP_PORT", "KYPOST_REPLICAS", "KYPOST_POSTGRES_PASSWORD", "KYPOST_POSTGRES_MAX_CONNECTIONS",
	"KYPOST_REPLICA_SUBNET", "KYPOST_PROXY_IP", "KYPOST_EDGE_TRUSTED_PROXY", // the replica overlay's Compose inputs
	"TRUST_PROXY_HEADERS", // retired; only warned about
	// Guarded by the tenancy record in meta instead (tenancy design K1): adding them to the
	// fingerprint would rewrite every upgraded install's record.
	"KYPOST_TENANCY", "KYPOST_STAFF_ORG_ID",
	// Receiving through Maddy is single-container; the replica profile refuses it (O2).
	"KYPOST_NATIVE_RECEIVER", "KYPOST_MADDY_BINARY", "KYPOST_RECEIVER_BINARY", "KYPOST_RECEIVING_CERT",
	"KYPOST_RECEIVING_KEY", "KYPOST_RECEIVING_LISTEN", "KYPOST_RECEIVING_HOSTNAME", "KYPOST_RECEIVING_TLS_DIR",
	"KYPOST_SMTP_BIND", "KYPOST_SMTP_PORT",
	// The hosted receiving tier's MX-host helper file; replicas never read it.
	"KYPOST_RECEIVING_REMOTE",
	// Child-process and test hooks, never set by an operator.
	"KYPOST_AUTHORITY_CHILD", "KYPOST_AUTHORITY_CONFIG", "KYPOST_MAILBOX_ACCOUNT", "KYPOST_MAILBOX_CHILD",
	"KYPOST_NATIVE_PROVISION_CONFIG", "KYPOST_NATIVE_PROVISION_MODE", "KYPOST_NATIVE_PROVISION_ROOT",
	"KYPOST_NATIVE_SEND_TEST_MODE", "KYPOST_NATIVE_SEND_TEST_PORT", "KYPOST_NATIVE_SEND_TEST_ROOT",
	"KYPOST_OUTBOUND_WORKER_TEST_PORT", "KYPOST_OUTBOUND_WORKER_TEST_ROOT", "KYPOST_OUTBOX_CRASH_DIR",
	"KYPOST_OUTBOX_SMTP_DIR", "KYPOST_OUTBOX_SMTP_MODE", "KYPOST_OUTBOX_SMTP_PORT", "KYPOST_PREPARE_CRASH_MODE",
	"KYPOST_PREPARE_CRASH_ROOT", "KYPOST_RELAY_CHECK_CHILD", "KYPOST_RELAY_TLS_PROOF_ADDR",
	"KYPOST_RELAY_TLS_PROOF_CASE", "KYPOST_TEST_DNS", "KYPOST_TEST_OLD_DATABASE_URL",
	"KYPOST_TEST_RESTORE_ENTROPY_DIR", "KYPOST_TEST_DATABASE_URL", "KYPOST_NATIVE_ANDROID_TEST_SERIAL",
	"KY_TEST_S3_ENDPOINT", "KY_TEST_S3_BUCKET", "KY_TEST_S3_ACCESS_KEY", "KY_TEST_S3_SECRET_KEY",
	"MADDY_PROOF_BINARY", "OLLAMA_LIVE_TEST", "RECEIVING_PROOF_IMAGE", "RSPAMD_PROOF", "UPDATE_AUTHORITY_GOLDEN",
}

func trim(v string) string  { return strings.TrimSpace(v) }
func lower(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

// list is a comma- or space-separated list sorted and deduplicated, so two replicas naming
// the same items in another order or spacing agree.
func list(v string) string {
	items := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	slices.Sort(items)
	return strings.Join(slices.Compact(items), ",")
}

// networks is list with each prefix or address parsed and masked (10.0.0.1/8 and 10.0.0.0/8
// agree; ::1 and ::1/128 agree). An unparsable item is kept as written: the server refuses it
// at startup anyway.
func networks(v string) string {
	items := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' })
	for i, item := range items {
		if p, err := netip.ParsePrefix(item); err == nil {
			items[i] = p.Masked().String()
		} else if a, err := netip.ParseAddr(item); err == nil {
			items[i] = netip.PrefixFrom(a, a.BitLen()).String()
		}
	}
	return list(strings.Join(items, ","))
}

// Fingerprint maps each shared setting's name to HMAC-SHA256 of its canonical value under an
// HKDF subkey of totp-secret.key, so reading the database reveals no value. KeyField holds
// the key's own check value. keys (table mode, MH5: name to path) adds each provisioned key
// file under its name, as the HMAC of the file's SHA-256, so every host must hold the same
// bytes; single mode passes none and its record is unchanged.
func Fingerprint(totpKeyPath string, getenv func(string) string, keys map[string]string) (map[string]string, error) {
	master, err := cryptutil.LoadKey(totpKeyPath)
	if err != nil {
		return nil, fmt.Errorf("replica configuration check: %s: %w", KeyField, err)
	}
	key, err := hkdf.Key(sha256.New, master, nil, "kypost:replica-config", 32)
	if err != nil {
		return nil, err
	}
	sum := func(name, value string) string {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(name + "\x00" + value))
		return hex.EncodeToString(m.Sum(nil))
	}
	out := map[string]string{KeyField: sum(KeyField, "")}
	for name, canon := range Shared {
		out[name] = sum(name, canon(getenv(name)))
	}
	for name, canon := range MultiShared {
		if v := canon(getenv(name)); v != "" {
			out[name] = sum(name, v)
		}
	}
	for name, path := range keys {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("replica configuration check: %s: %w", name, err)
		}
		digest := sha256.Sum256(b)
		out[name] = sum(name, hex.EncodeToString(digest[:]))
	}
	return out, nil
}

// Diff names the settings whose fingerprints differ, sorted, never their values. A differing
// key is reported alone. A name only one side knows is no difference: an upgrade adds it to
// the record (Merge) and a removed one no longer matters.
func Diff(stored, current map[string]string) []string {
	if !hmac.Equal([]byte(stored[KeyField]), []byte(current[KeyField])) {
		return []string{KeyField}
	}
	var out []string
	for name, v := range current {
		if old, ok := stored[name]; ok && !hmac.Equal([]byte(old), []byte(v)) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// ErrConfigMismatch is a replica whose shared settings differ from the deployment's record.
var ErrConfigMismatch = errors.New("this replica's shared settings differ from the deployment's")

// CheckConfig records this process's fingerprint on the first PostgreSQL start and refuses a
// later start whose shared settings differ, naming the settings only. A setting the record
// lacks (added by an upgrade) is merged in. It runs under the replica-config lock, so two
// first starts with different settings record one and refuse the other.
func CheckConfig(ctx context.Context, st *state.Store, totpKeyPath string, keys map[string]string) error {
	current, err := Fingerprint(totpKeyPath, os.Getenv, keys)
	if err != nil {
		return err
	}
	return st.Serialized(ctx, "replica-config", func(context.Context) error {
		raw, err := st.Setting(FingerprintSetting)
		if errors.Is(err, state.ErrNotFound) {
			b, _ := json.Marshal(current)
			return st.SetSetting(FingerprintSetting, string(b))
		}
		if err != nil {
			return fmt.Errorf("replica configuration check: %w", err)
		}
		var stored map[string]string
		if err := json.Unmarshal([]byte(raw), &stored); err != nil {
			return fmt.Errorf("replica configuration check: the stored record is unreadable: %w", err)
		}
		if diff := Diff(stored, current); len(diff) > 0 {
			return mismatch(diff)
		}
		added := false
		for name, v := range current {
			if _, ok := stored[name]; !ok {
				stored[name], added = v, true
			}
		}
		if !added {
			return nil
		}
		b, _ := json.Marshal(stored)
		return st.SetSetting(FingerprintSetting, string(b))
	})
}

func mismatch(fields []string) error {
	if len(fields) == 1 && fields[0] == KeyField {
		return fmt.Errorf("%w: %s is not the deployment's key; every replica must mount the same private volume", ErrConfigMismatch, KeyField)
	}
	return fmt.Errorf("%w: %s. Make them equal on every replica, or after a deliberate change stop every replica and run kypost-server accept-replica-config", ErrConfigMismatch, strings.Join(fields, ", "))
}

// Accept rewrites the record from this process's settings after a deliberate change and
// returns the names that changed. It refuses a different totp-secret.key. The caller makes
// sure no replica is live.
func Accept(ctx context.Context, st *state.Store, totpKeyPath string, keys map[string]string) ([]string, error) {
	current, err := Fingerprint(totpKeyPath, os.Getenv, keys)
	if err != nil {
		return nil, err
	}
	var changed []string
	err = st.Serialized(ctx, "replica-config", func(context.Context) error {
		raw, err := st.Setting(FingerprintSetting)
		if err != nil && !errors.Is(err, state.ErrNotFound) {
			return err
		}
		if err == nil {
			var stored map[string]string
			if err := json.Unmarshal([]byte(raw), &stored); err == nil {
				if !hmac.Equal([]byte(stored[KeyField]), []byte(current[KeyField])) {
					return mismatch([]string{KeyField})
				}
				changed = Diff(stored, current)
			}
		}
		b, _ := json.Marshal(current)
		return st.SetSetting(FingerprintSetting, string(b))
	})
	return changed, err
}
