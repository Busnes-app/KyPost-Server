package config

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	crand "crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/Busnes-app/kypost-server/backend/internal/fsutil"

	"gopkg.in/yaml.v3"
)

type Paths struct {
	ConfigFile string
	StateDir   string
}

// The container's data directories. These literals used to be copy-pasted as
// EnvOrDefault fallbacks across eight packages and twenty-odd call sites, and
// several secret files hardcoded "/kypost/private/..." independently of
// SECRET_DIR — so overriding SECRET_DIR moved some secrets and not others,
// and any code path that forgot one env var silently reached for the host's
// root directory instead of failing. Declare them once.
const (
	DefaultConfigDir = "/kypost/config"
	DefaultStateDir  = "/kypost/state"
	DefaultLogDir    = "/kypost/logs"
	DefaultSecretDir = "/kypost/private"
)

// ConfigDir, StateDir, LogDir and SecretDir resolve the four data directories
// from the environment, falling back to the container defaults above.
func ConfigDir() string { return EnvOrDefault("CONFIG_DIR", DefaultConfigDir) }
func StateDir() string  { return EnvOrDefault("STATE_DIR", DefaultStateDir) }
func LogDir() string    { return EnvOrDefault("LOG_DIR", DefaultLogDir) }
func SecretDir() string { return EnvOrDefault("SECRET_DIR", DefaultSecretDir) }

// SecretFile resolves the path to a single secret. A per-secret env override
// (e.g. PGP_PRIVATE_KEY_FILE) still wins so existing deployments that point
// individual keys elsewhere keep working, but the fallback is now derived
// from SecretDir() rather than being its own hardcoded absolute path. That is
// what makes SECRET_DIR actually relocate every secret — including in tests,
// which is why this exists.
func SecretFile(envKey, name string) string {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v
	}
	return filepath.Join(SecretDir(), name)
}

// EnvOrDefault returns the trimmed value of the environment variable key, or
// fallback if it is unset or blank after trimming.
func EnvOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// IntEnv is an integer environment variable: unset or blank means Default,
// anything else must be a whole number in [Min, Max].
type IntEnv struct {
	Key      string
	Default  int
	Min, Max int
}

// Integer environment variables, declared once so CheckEnv and every reader agree.
var (
	WebPort                 = IntEnv{"WEB_PORT", 5866, 1, 65535}
	SMTPPort                = IntEnv{"SMTP_PORT", 587, 1, 65535}
	PoWMaxNumber            = IntEnv{"POW_MAX_NUMBER", 0, 1, math.MaxInt32} // 0: captcha's own default
	UnhealthyRestartSeconds = IntEnv{"UNHEALTHY_RESTART_SECONDS", 300, 1, math.MaxInt32}
	ClassifyConcurrency     = IntEnv{"CLASSIFY_CONCURRENCY", 1, 1, 1024}
	ClassifyPaceMS          = IntEnv{"CLASSIFY_PACE_MS", 0, 0, 3_600_000}
)

// Get reads e. A set but malformed or out-of-range value is an error naming the variable.
func (e IntEnv) Get() (int, error) {
	raw := strings.TrimSpace(os.Getenv(e.Key))
	if raw == "" {
		return e.Default, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < e.Min || v > e.Max {
		return e.Default, fmt.Errorf("%s=%q is not valid: use a whole number from %d to %d, or leave it unset for %d", e.Key, raw, e.Min, e.Max, e.Default)
	}
	return v, nil
}

// EnvBool reads key as "true" or "false"; unset or blank is false. Anything
// else is an error, so a typo cannot silently pick a mode.
func EnvBool(key string) (bool, error) {
	switch strings.TrimSpace(os.Getenv(key)) {
	case "", "false":
		return false, nil
	case "true":
		return true, nil
	default:
		return false, errors.New(key + " must be true or false")
	}
}

// CheckEnv parses every typed environment variable, so the api and daemon
// processes refuse the same bad value at startup instead of defaulting.
func CheckEnv() error {
	var bad []string
	for _, e := range []IntEnv{WebPort, SMTPPort, PoWMaxNumber, UnhealthyRestartSeconds, ClassifyConcurrency, ClassifyPaceMS} {
		if _, err := e.Get(); err != nil {
			bad = append(bad, err.Error())
		}
	}
	for _, key := range []string{"ALLOW_INSECURE_SMTP", "MFA_CLEAR_ALL", "KYPOST_NATIVE_RECEIVER", "KYPOST_DISABLE_AI_SORTING"} {
		if _, err := EnvBool(key); err != nil {
			bad = append(bad, err.Error())
		}
	}
	if len(bad) > 0 {
		// One line: the logger escapes newlines.
		return errors.New(strings.Join(bad, "; "))
	}
	return nil
}

type Config struct {
	Timezone string `yaml:"timezone" json:"timezone"`
	LogLevel string `yaml:"logLevel" json:"logLevel"`

	Scan struct {
		IntervalSeconds int `yaml:"intervalSeconds" json:"intervalSeconds"`
	} `yaml:"scan" json:"scan"`

	RateLimits struct {
		PerMinute int `yaml:"perMinute" json:"perMinute"`
		PerHour   int `yaml:"perHour" json:"perHour"`
	} `yaml:"rateLimits" json:"rateLimits"`

	SendQuota SendQuota `yaml:"sendQuota" json:"sendQuota"`

	Redaction struct {
		Patterns []Pattern `yaml:"patterns" json:"patterns"`
	} `yaml:"redaction" json:"redaction"`

	Labels struct {
		Allowlist       []string            `yaml:"allowlist" json:"allowlist"`
		KeywordMappings map[string][]string `yaml:"keywordMappings" json:"keywordMappings"`
	} `yaml:"labels" json:"labels"`

	Notifications NotificationKeys `yaml:"notifications" json:"-"`
}

// NotificationKeys is the shared VAPID signing identity for the whole
// install. Delivery preferences (mode/keywords) are per-user and live in
// UserSettings instead.
type NotificationKeys struct {
	PublicKey      string `yaml:"publicKey" json:"-"`
	PrivateKeyPath string `yaml:"privateKeyPath" json:"-"`
}

// UserSettings is the small per-user preferences document stored at
// CONFIG_DIR/users/<userID>/config.yaml.
type UserSettings struct {
	EncryptIncoming bool                     `yaml:"encryptIncoming" json:"encryptIncoming"`
	Notifications   UserNotificationSettings `yaml:"notifications" json:"notifications"`
	Labels          UserLabelSettings        `yaml:"labels" json:"labels"`
}

type UserNotificationSettings struct {
	Mode     string   `yaml:"mode" json:"mode"`
	Keywords []string `yaml:"keywords" json:"keywords"`
	// ContentPreview controls whether the sender and subject of a message
	// are placed in the push payload, or whether the notification is
	// generic ("KyPost" / "You have a new email.").
	//
	// It defaults to FALSE, and that default is the point. A push
	// notification is not delivered by this server: it travels
	// backend -> Cloudflare Worker relay -> Google FCM or Apple APNs, in
	// cleartext to each of those hops. Putting the sender and subject in it
	// hands the correspondence graph of a self-hosted, PGP-advertising mail
	// product to exactly the third parties its users chose self-hosting to
	// avoid. Encrypting the body buys nothing if the Subject header is
	// couriered to Google on arrival.
	//
	// Users who want previews can have them — this is their mail and their
	// call — but it must be a decision they made, not a default they never
	// saw. See the copy on Configuration's Notifications tab.
	//
	// Note the absence of omitempty: false must serialize, or a user who
	// turns previews off round-trips back to "unset" and the field reads as
	// absent rather than as the choice they made. (Absent still means
	// false, so an older settings file is private by default too.)
	ContentPreview bool `yaml:"contentPreview" json:"contentPreview"`
}

// UserLabelSettings controls whether the AI classification pipeline
// automatically applies keyword labels for this user. When
// AutoApplyEnabled is false, classification is skipped entirely and every
// message is tagged with the account's default label instead (see
// disabledLabelingFallback in processor/poller.go).
type UserLabelSettings struct {
	AutoApplyEnabled bool `yaml:"autoApplyEnabled" json:"autoApplyEnabled"`

	// Seeded records that this account's label set has been copied from the
	// house list. It exists because "no labels" and "never set up" are
	// different states that a slice cannot tell apart across a YAML
	// round-trip: yaml.Marshal writes a nil slice as `[]`, which reads back
	// as an empty non-nil one. Without this flag an account that
	// deliberately cleared every label would have the house list poured back
	// in on the next load, forever.
	Seeded bool `yaml:"seeded" json:"seeded"`

	// Allowlist is this account's own label set — the labels the classifier
	// may choose from, applied as IMAP keywords verbatim.
	Allowlist []string `yaml:"allowlist" json:"allowlist"`

	// KeywordMappings expands one chosen label into several IMAP keywords.
	KeywordMappings map[string][]string `yaml:"keywordMappings" json:"keywordMappings"`

	// Descriptions is an optional one-line description per label. The
	// embedding sorter uses it as that label's first example, so a label the
	// user adds is predictable before they have corrected any mail into it.
	// It is never sent to the LLM.
	Descriptions map[string]string `yaml:"descriptions,omitempty" json:"descriptions"`
}

// MaxLabelDescriptionRunes bounds one label description.
const MaxLabelDescriptionRunes = 200

func DefaultUserSettings() UserSettings {
	var s UserSettings
	s.Notifications.Mode = "none"
	s.Notifications.Keywords = []string{}
	s.Labels.AutoApplyEnabled = true
	s.Labels.Allowlist = []string{}
	s.Labels.KeywordMappings = map[string][]string{}
	s.Labels.Descriptions = map[string]string{}
	return s
}

// SeedUserLabels copies the instance house list into an account that has never
// had one, reporting whether it changed anything.
//
// The house list (`labels` in config.yaml, edited by an admin) is the starting
// point for a new account and the migration path for every account that
// existed before labels became per-user — those accounts were classified
// against the house list, so adopting it verbatim is the only change that
// leaves their mail sorted the way it already was.
//
// After seeding, the two are independent: editing the house list does not
// reach back into an account that already has its own.
func SeedUserLabels(s *UserSettings, house Config) bool {
	if s.Labels.Seeded {
		return false
	}
	s.Labels.Seeded = true
	s.Labels.Allowlist = append([]string{}, house.Labels.Allowlist...)
	s.Labels.KeywordMappings = map[string][]string{}
	for label, keywords := range house.Labels.KeywordMappings {
		s.Labels.KeywordMappings[label] = append([]string{}, keywords...)
	}
	return true
}

// SafeUserPathComponent keeps malformed or legacy IDs inside the directory they
// name an entry of. Valid IDs are opaque UUIDs, but path construction must not
// rely on that surviving hand-edited or migrated users.json files.
func SafeUserPathComponent(id string) string {
	if fsutil.SafePathComponent(id) {
		return id
	}
	return "__invalid-user-id__"
}

// UserDir is a user's directory under a config or state root.
func UserDir(root, userID string) string {
	return filepath.Join(root, "users", SafeUserPathComponent(userID))
}

// Per-user files under the config root.
func UserIMAPConfigPath(configDir, userID string) string {
	return filepath.Join(UserDir(configDir, userID), "imap-config.json")
}

func UserTuningPath(configDir, userID string) string {
	return filepath.Join(UserDir(configDir, userID), "tuning.md")
}

func UserSettingsPath(configDir, userID string) string {
	return filepath.Join(UserDir(configDir, userID), "config.yaml")
}

// LoadUserLabelSettings reads an account's settings and seeds its labels from
// the house list on first sight, persisting the seed so it happens once.
//
// Callers that only read labels should use this rather than LoadUserSettings,
// so a pre-existing account gets its list the first time anything asks —
// there is no separate migration step to run or to forget.
func LoadUserLabelSettings(path string, house Config) (UserSettings, error) {
	s, err := LoadUserSettings(path)
	if err != nil {
		return UserSettings{}, err
	}
	if s.Labels.Seeded {
		return s, nil
	}
	// Re-seed inside the lock rather than saving the copy read above: another
	// process may have seeded between the read and here.
	if err := UpdateUserSettings(path, func(locked *UserSettings) error {
		SeedUserLabels(locked, house)
		return nil
	}); err != nil {
		// Seeding is a convenience, not a precondition. Answer with the
		// in-memory seed so classification still works on a read-only or
		// full disk; the next call will try to persist again.
		SeedUserLabels(&s, house)
		return s, err
	}
	return LoadUserSettings(path)
}

// LoadUserSettings reads a per-user settings file, returning defaults if it
// does not exist yet.
func LoadUserSettings(path string) (UserSettings, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultUserSettings(), nil
		}
		return UserSettings{}, err
	}
	s := DefaultUserSettings()
	if err := yaml.Unmarshal(b, &s); err != nil {
		return UserSettings{}, err
	}
	if s.Notifications.Keywords == nil {
		s.Notifications.Keywords = []string{}
	}
	if s.Labels.Allowlist == nil {
		s.Labels.Allowlist = []string{}
	}
	if s.Labels.KeywordMappings == nil {
		s.Labels.KeywordMappings = map[string][]string{}
	}
	if s.Labels.Descriptions == nil {
		s.Labels.Descriptions = map[string]string{}
	}
	return s, nil
}

func SaveUserSettings(path string, s UserSettings) error {
	b, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, b, 0o600)
}

// UpdateUserSettings applies mutate to the settings at path as one
// read-modify-write cycle, holding an inter-process file lock across the whole
// cycle.
//
// Load-then-Save from a handler is not enough, and this was the only store in
// the project with neither a mutex nor a file lock. AtomicWriteFile means a
// reader never sees a torn file, but atomicity is not serialization: the two
// preference handlers each load the whole settings struct, replace one section
// and write it all back, so a PUT to /api/labels/preferences landing between
// another request's load and save silently reverts whatever that request set —
// including the contentPreview privacy opt-out, which is worse than never
// having offered it, because the user ticked the box and believes it.
//
// A missing file starts from DefaultUserSettings rather than the zero value, so
// sections the caller does not touch keep their intended defaults instead of
// being written as false/empty. Nothing is persisted if mutate returns an
// error.
//
// A read that FAILS is not a missing file and must not be treated as one.
// LoadUserSettings already answers "no file yet" with the defaults, so every
// error left here is a real one — an unreadable file, or YAML that no longer
// parses — and starting from defaults after one meant the next preference PUT
// silently reset notification mode, keywords and the contentPreview privacy
// opt-out, then overwrote the only copy of the document that would have shown
// what they were.
func UpdateUserSettings(path string, mutate func(*UserSettings) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir user settings dir: %w", err)
	}
	return fsutil.WithFileLock(path, func() error {
		s, err := LoadUserSettings(path)
		if err != nil {
			return fmt.Errorf("load user settings: %w", err)
		}
		if err := mutate(&s); err != nil {
			return err
		}
		return SaveUserSettings(path, s)
	})
}

// LoadLegacyNotificationPrefs extracts the pre-multi-user mode/keywords
// fields from a legacy global config.yaml, for one-time migration into the
// first admin user's settings file.
func LoadLegacyNotificationPrefs(path string) (UserNotificationSettings, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return UserNotificationSettings{}, false
	}
	var legacy struct {
		Notifications UserNotificationSettings `yaml:"notifications"`
	}
	if err := yaml.Unmarshal(b, &legacy); err != nil {
		return UserNotificationSettings{}, false
	}
	if strings.TrimSpace(legacy.Notifications.Mode) == "" {
		return UserNotificationSettings{}, false
	}
	return legacy.Notifications, true
}

// SendQuota bounds how much mail one user sends, over every send path.
type SendQuota struct {
	MessagesPerHour  int `yaml:"messagesPerHour" json:"messagesPerHour"`
	RecipientsPerDay int `yaml:"recipientsPerDay" json:"recipientsPerDay"`
}

func (q SendQuota) Valid() bool { return q.MessagesPerHour > 0 && q.RecipientsPerDay > 0 }

type Pattern struct {
	Name        string `yaml:"name" json:"name"`
	Regex       string `yaml:"regex" json:"regex"`
	Replacement string `yaml:"replacement" json:"replacement"`
}

func Default() Config {
	cfg := Config{
		Timezone: "America/New_York",
		LogLevel: "info",
	}
	cfg.Scan.IntervalSeconds = 90
	cfg.RateLimits.PerMinute = 10
	cfg.RateLimits.PerHour = 20
	cfg.SendQuota = SendQuota{MessagesPerHour: 60, RecipientsPerDay: 1000}
	// Order matters: each pattern runs over the previous one's output. IBAN
	// and card go before the national phone form, which would otherwise take
	// a digit run out of their middle. \b is ASCII-only in RE2, so the Unicode
	// email pattern goes without it.
	cfg.Redaction.Patterns = []Pattern{
		{Name: "email", Regex: `(?i)[\p{L}\p{M}\p{N}_.%+'-]+@[\p{L}\p{M}\p{N}-]+(?:\.[\p{L}\p{M}\p{N}-]+)*\.(?:xn--[\p{L}\p{N}-]+|[\p{L}\p{M}]{2,})`, Replacement: "[REDACTED_EMAIL]"},
		// The 0.4 phone rule in its 0.4 place (3-3-4 with an optional country
		// code: 15551234567, 1-800-555-0199), so an upgrade un-redacts nothing
		// and no later rule can cut into its numbers. Unlike 0.4 it also takes
		// a leading + that follows a space.
		{Name: "phone_us", Regex: `(?:\+\d{1,3}[\s.-]?|\b(?:\+?\d{1,3}[\s.-]?)?)(?:\(\d{3}\)|\d{3})[\s.-]?\d{3}[\s.-]?\d{4}\b`, Replacement: "[REDACTED_PHONE]"},
		// Unbroken, or spaced in fours; after the bank code a spaced group
		// needs a digit, so "RT66 road trip" stays prose.
		{Name: "iban", Regex: `(?i)\b[A-Z]{2}\d{2}(?:[A-Z0-9]{11,30}\b| [A-Z0-9]{4}(?: (?:\d[A-Z0-9]{3}|[A-Z0-9]{3}\d)){1,6}(?: [A-Z0-9]{0,3}\d)?)`, Replacement: "[REDACTED_IBAN]"},
		// E.164 after + or 00, then every digit that follows: stopping at 15
		// would leave the rest of an adjacent number behind. Before card so
		// 13-15 digits keep this label.
		{Name: "phone_intl", Regex: `(?:\+|\b00)\d(?:[ .()-]{0,2}\d){6,}`, Replacement: "[REDACTED_PHONE]"},
		// 13 or more digits, greedy to the end of the run for the same reason.
		{Name: "card", Regex: `\b\d(?:[ -]*\d){12,}\b`, Replacement: "[REDACTED_CARD]"},
		// A bare 9-digit number is an SSN only after the word SSN; otherwise
		// every order and tracking number would go.
		{Name: "ssn", Regex: `\b\d{3}-\d{2}-\d{4}\b|\b\d{3} \d{2} \d{4}\b|(?i:\bssn|\bsocial security(?: number| no\.?)?)[\s:#]*\d{9}\b`, Replacement: "[REDACTED_SSN]"},
		// (555) after a space, or a 0-led area code then 7-12 digits, which no
		// dd-mm-yyyy date reaches.
		{Name: "phone", Regex: `(?:\(\d{3}\) ?|\b\d{3}[ .-]?)\d{3}[ .-]?\d{4}\b|(?:\(0\d{1,4}\) ?|\b0\d{1,4}[ /-]?)\d(?: ?\d){6,11}\b`, Replacement: "[REDACTED_PHONE]"},
	}
	cfg.Labels.KeywordMappings = map[string][]string{}
	return cfg
}

func LoadOrInit(path string) (Config, error) {
	configDir := filepath.Dir(path)
	// Same SECRET_DIR convention processor.relayKeyFilePathWithPrefix uses.
	secretDir := SecretDir()
	if _, err := os.Stat(path); err == nil {
		cfg, err := Load(path)
		if err != nil {
			return Config{}, err
		}
		changed, err := ensureNotificationKeyMaterial(configDir, secretDir, &cfg)
		if err != nil {
			return Config{}, err
		}
		// A saved list replaces the defaults whole, so an unedited old default
		// list is upgraded here; an edited one is the admin's and stays.
		if slices.Equal(cfg.Redaction.Patterns, redactionDefaultsV04) || slices.Equal(cfg.Redaction.Patterns, redactionDefaultsV01) {
			cfg.Redaction.Patterns = Default().Redaction.Patterns
			changed = true
			slog.Info("config.yaml: replaced unedited old redaction defaults with the current ones")
		}
		if changed {
			if err := Save(path, cfg); err != nil {
				return Config{}, err
			}
		}
		return cfg, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Config{}, fmt.Errorf("mkdir config dir: %w", err)
	}
	cfg := Default()
	_, err := ensureNotificationKeyMaterial(configDir, secretDir, &cfg)
	if err != nil {
		return Config{}, err
	}
	if err := Save(path, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// redactionDefaultsV04 is Default()'s redaction list from 0.3.0 through 0.4.0.
var redactionDefaultsV04 = []Pattern{
	{Name: "email", Regex: `(?i)\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b`, Replacement: "[REDACTED_EMAIL]"},
	{Name: "phone", Regex: `\b(?:\+?\d{1,3}[\s.-]?)?(?:\(\d{3}\)|\d{3})[\s.-]?\d{3}[\s.-]?\d{4}\b`, Replacement: "[REDACTED_PHONE]"},
	{Name: "ssn", Regex: `\b\d{3}-\d{2}-\d{4}\b`, Replacement: "[REDACTED_SSN]"},
	{Name: "iban", Regex: `\b[A-Z]{2}\d{2}[A-Z0-9]{10,30}\b`, Replacement: "[REDACTED_IBAN]"},
	{Name: "card", Regex: `\b(?:\d[ -]*?){13,19}\b`, Replacement: "[REDACTED_CARD]"},
}

// redactionDefaultsV01 is Default()'s list before 7e0b788 (2026-07-17). Its
// doubled backslashes match nothing, so those installs redacted nothing.
var redactionDefaultsV01 = []Pattern{
	{Name: "email", Regex: `(?i)\\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\\.[A-Z]{2,}\\b`, Replacement: "[REDACTED_EMAIL]"},
	{Name: "phone", Regex: `\\b(?:\\+?\\d{1,3}[\\s.-]?)?(?:\\(\\d{3}\\)|\\d{3})[\\s.-]?\\d{3}[\\s.-]?\\d{4}\\b`, Replacement: "[REDACTED_PHONE]"},
	{Name: "ssn", Regex: `\\b\\d{3}-\\d{2}-\\d{4}\\b`, Replacement: "[REDACTED_SSN]"},
	{Name: "iban", Regex: `\\b[A-Z]{2}\\d{2}[A-Z0-9]{10,30}\\b`, Replacement: "[REDACTED_IBAN]"},
	{Name: "card", Regex: `\\b(?:\\d[ -]*?){13,19}\\b`, Replacement: "[REDACTED_CARD]"},
}

// Load reads config.yaml strictly: an unknown key is an error naming it, so a
// misspelled setting cannot silently run on its default. Keys older versions
// wrote are dropped first, so an upgrade still starts.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	b := raw
	if dropRetiredKeys(&doc) {
		if b, err = yaml.Marshal(&doc); err != nil {
			return Config{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	cfg := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		var te *yaml.TypeError
		if errors.As(err, &te) {
			return Config{}, fmt.Errorf("%s: %s", path, strings.Join(te.Errors, "; "))
		}
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	warnRetiredClassifier(raw)
	return cfg, nil
}

// dropRetiredKeys removes keys earlier versions wrote to config.yaml; the next
// Save leaves them out of the file.
func dropRetiredKeys(doc *yaml.Node) bool {
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return false
	}
	top := doc.Content[0]
	dropped := dropKeys(top, "llama", "classifier")
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value == "notifications" && top.Content[i+1].Kind == yaml.MappingNode {
			dropped = dropKeys(top.Content[i+1], "mode", "keywords", "folder") || dropped
		}
	}
	return dropped
}

func dropKeys(m *yaml.Node, keys ...string) (dropped bool) {
	kept := m.Content[:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if slices.Contains(keys, m.Content[i].Value) {
			dropped = true
			continue
		}
		kept = append(kept, m.Content[i], m.Content[i+1])
	}
	m.Content = kept
	return dropped
}

// warnRetiredClassifier says so out loud when config.yaml still carries the
// retired `classifier:` block.
//
// Load drops the block, so an install that pointed classification at a remote
// endpoint keeps starting — but it is now classifying against the OLLAMA_* env
// vars instead, with no other symptom.
// A silent change of where email content is sent deserves a line in the log.
func warnRetiredClassifier(raw []byte) {
	var probe struct {
		Classifier map[string]any `yaml:"classifier"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil || len(probe.Classifier) == 0 {
		return
	}
	slog.Warn("ignoring the retired `classifier:` block in config.yaml; " +
		"classification now uses the OLLAMA_* environment variables only. " +
		"Remove the block to silence this warning.")
}

func Save(path string, cfg Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	// Atomic temp+rename like every other persisted file in this project.
	// os.WriteFile truncates in place, and the daemon calls Load on every
	// tick — a read landing inside that window sees an empty file, which
	// unmarshals successfully into the zero value, so Load silently returns
	// Default() rather than an error. That drops any operator-configured
	// redaction pattern from what gets fed to the classifier, for a tick.
	return fsutil.AtomicWriteFile(path, b, 0o600)
}

// ensureNotificationKeyMaterial fills in the VAPID identity, generating the
// private key on first use.
//
// New installs put the key in secretDir. It used to go to configDir, which made
// it the one secret in this system outside SECRET_DIR — those are separate
// volumes with separate lifecycles, and an operator who copies "the config"
// reasonably does not expect a signing key to come with it.
//
// An install that already records a path keeps it, which is why the assignment
// is guarded rather than unconditional. The VAPID public key is registered with
// every browser that has ever subscribed to notifications, so relocating an
// existing key would invalidate every live subscription — a worse outcome than
// the key sitting in a directory it should not have been in. New installs get
// the right layout; existing ones keep working.
func ensureNotificationKeyMaterial(configDir, secretDir string, cfg *Config) (bool, error) {
	changed := false
	if strings.TrimSpace(cfg.Notifications.PrivateKeyPath) == "" {
		dir := strings.TrimSpace(secretDir)
		if dir == "" {
			dir = configDir
		}
		cfg.Notifications.PrivateKeyPath = filepath.Join(dir, "notifications-vapid-private.pem")
		changed = true
	}
	key, err := loadOrCreateNotificationPrivateKey(cfg.Notifications.PrivateKeyPath)
	if err != nil {
		return changed, err
	}
	// elliptic.Marshal is deprecated since Go 1.21. ECDH().Bytes() produces
	// the identical uncompressed SEC 1 point (0x04 || X || Y) that VAPID
	// requires, so this is an encoding-compatible swap, not a format change.
	ecdhPub, err := key.PublicKey.ECDH()
	if err != nil {
		return changed, fmt.Errorf("encode notification public key: %w", err)
	}
	publicKey := base64.RawURLEncoding.EncodeToString(ecdhPub.Bytes())
	if cfg.Notifications.PublicKey != publicKey {
		cfg.Notifications.PublicKey = publicKey
		changed = true
	}
	return changed, nil
}

// LoadVAPIDPrivateKey reads the notification VAPID private key PEM at path and
// returns it in the base64url raw-scalar form the webpush library expects.
func LoadVAPIDPrivateKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return "", fmt.Errorf("vapid pem block missing")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return "", err
	}
	// ecdh.PrivateKey.Bytes() returns the scalar already fixed-width and
	// left-padded, which is what VAPID wants. key.D.Bytes() (deprecated since
	// Go 1.26) strips leading zeros, hence the manual padding this replaces.
	ecdhKey, err := key.ECDH()
	if err != nil {
		return "", fmt.Errorf("convert vapid private key: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(ecdhKey.Bytes()), nil
}

func parseNotificationPrivateKeyPEM(b []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, fmt.Errorf("decode notification private key: pem block missing")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse notification private key: %w", err)
	}
	return key, nil
}

// loadOrCreateNotificationPrivateKey reads the VAPID private key at path,
// generating and persisting a new one on first run.
//
// daemon and server are separate OS processes that each independently call
// this on boot. On a fresh install both can observe "file doesn't exist"
// and race to generate + write a keypair; whichever write lands last "wins"
// on disk while the other process would otherwise keep running with a
// DIFFERENT keypair in memory, so the persisted public key in config.yaml
// could end up not matching the private key on disk. To prevent that, the
// whole read-check-generate-write sequence is guarded by a syscall.Flock on
// a sibling lock file. Flock (rather than an O_EXCL lock file) is used
// deliberately: it is released automatically by the kernel if the holding
// process dies, so a crash mid-lock can never deadlock a future boot. The
// losing side of the race re-reads the file after acquiring the lock
// instead of trusting its own already-generated in-memory key, so every
// caller converges on the single keypair that actually ends up on disk.
func loadOrCreateNotificationPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir notification key dir: %w", err)
	}

	lockFile, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open notification key lock: %w", err)
	}
	defer lockFile.Close()

	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock notification key: %w", err)
	}
	// Unlock errors are not actionable: the deferred Close below releases the
	// lock regardless, and the process is on its way out of this function
	// either way. Discard explicitly rather than implicitly.
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }()

	// Re-check under the lock: another process/goroutine may have already
	// generated and written the key while we were waiting for the lock. If
	// so, use what's actually on disk rather than generating our own.
	if b, err := os.ReadFile(path); err == nil {
		return parseNotificationPrivateKeyPEM(b)
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), crand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate notification key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("marshal notification key: %w", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := fsutil.AtomicWriteFile(path, pemBytes, 0o600); err != nil {
		return nil, fmt.Errorf("write notification key: %w", err)
	}
	return key, nil
}
