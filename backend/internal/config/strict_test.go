package config

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIntEnvRefusesMalformedValues(t *testing.T) {
	for value, want := range map[string]int{"": 5866, "  ": 5866, "8080": 8080, " 9000 ": 9000, "abc": -1, "0": -1, "65536": -1, "-1": -1, "80.5": -1} {
		t.Setenv("WEB_PORT", value)
		got, err := WebPort.Get()
		if want == -1 {
			if err == nil || !strings.Contains(err.Error(), "WEB_PORT") {
				t.Errorf("%q: got %d, %v; want an error naming WEB_PORT", value, got, err)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: got %d, %v; want %d", value, got, err, want)
		}
	}
}

func TestCheckEnvNamesEveryBadVariable(t *testing.T) {
	if err := CheckEnv(); err != nil {
		t.Fatalf("unset environment: %v", err)
	}
	t.Setenv("SMTP_PORT", "smtp")
	t.Setenv("CLASSIFY_PACE_MS", "-5")
	t.Setenv("ALLOW_INSECURE_SMTP", "TRUE")
	t.Setenv("KYPOST_DISABLE_AI_SORTING", "yes")
	err := CheckEnv()
	for _, key := range []string{"SMTP_PORT", "CLASSIFY_PACE_MS", "ALLOW_INSECURE_SMTP", "KYPOST_DISABLE_AI_SORTING"} {
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("CheckEnv() = %v; want it to name %s", err, key)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRefusesUnknownKeys(t *testing.T) {
	for _, tc := range []struct{ body, key string }{
		{"redacton:\n  patterns: []\n", "redacton"},
		{"scan:\n  intervalSecond: 5\n", "intervalSecond"},
		{"notifications:\n  mode: all\n  publicKeys: x\n", "publicKeys"},
	} {
		_, err := Load(writeConfig(t, tc.body))
		if err == nil || !strings.Contains(err.Error(), tc.key) {
			t.Errorf("Load(%q) = %v; want an error naming %s", tc.body, err, tc.key)
		}
	}
}

// Keys older versions wrote must not stop an upgraded install from starting.
func TestLoadAcceptsRetiredKeys(t *testing.T) {
	cfg, err := Load(writeConfig(t, `timezone: Europe/Berlin
llama:
  baseUrl: http://old
classifier:
  baseUrl: http://old
notifications:
  mode: keywords
  keywords: [urgent]
  folder: INBOX
  publicKey: pub
scan:
  intervalSeconds: 30
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timezone != "Europe/Berlin" || cfg.Notifications.PublicKey != "pub" || cfg.Scan.IntervalSeconds != 30 {
		t.Fatalf("retired keys disturbed the rest: %+v", cfg)
	}
	if cfg, err := Load(writeConfig(t, "")); err != nil || cfg.Scan.IntervalSeconds != Default().Scan.IntervalSeconds {
		t.Fatalf("empty file: %+v, %v", cfg, err)
	}
}

// initWithPatterns writes a config.yaml carrying patterns and returns its path.
func initWithPatterns(t *testing.T, patterns []Pattern) string {
	t.Helper()
	t.Setenv("SECRET_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg, err := LoadOrInit(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Redaction.Patterns = patterns
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadOrInitUpgradesUneditedRedactionDefaults(t *testing.T) {
	path := initWithPatterns(t, slices.Clone(redactionDefaultsV04))
	cfg, err := LoadOrInit(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Default().Redaction.Patterns
	if !slices.Equal(cfg.Redaction.Patterns, want) {
		t.Fatalf("patterns not upgraded: %+v", cfg.Redaction.Patterns)
	}
	if onDisk, err := Load(path); err != nil || !slices.Equal(onDisk.Redaction.Patterns, want) {
		t.Fatalf("upgrade not persisted: %+v, %v", onDisk.Redaction.Patterns, err)
	}
	assertLoadOrInitIdempotent(t, path)
}

// testdata/config-pre-7e0b788.yaml is Save(Default()) from 7e0b788^, whose
// doubled-backslash patterns matched nothing.
func TestLoadOrInitUpgradesPre7e0b788Config(t *testing.T) {
	raw, err := os.ReadFile("testdata/config-pre-7e0b788.yaml")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECRET_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(old.Redaction.Patterns, redactionDefaultsV01) {
		t.Fatalf("historical file does not load as redactionDefaultsV01: %+v", old.Redaction.Patterns)
	}
	cfg, err := LoadOrInit(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Default().Redaction.Patterns
	if !slices.Equal(cfg.Redaction.Patterns, want) {
		t.Fatalf("patterns not upgraded: %+v", cfg.Redaction.Patterns)
	}
	if onDisk, err := Load(path); err != nil || !slices.Equal(onDisk.Redaction.Patterns, want) {
		t.Fatalf("upgrade not persisted: %+v, %v", onDisk.Redaction.Patterns, err)
	}
	assertLoadOrInitIdempotent(t, path)

	edited := slices.Clone(redactionDefaultsV01)
	edited[4].Replacement = "[CARD]"
	path = initWithPatterns(t, edited)
	if cfg, err := LoadOrInit(path); err != nil || !slices.Equal(cfg.Redaction.Patterns, edited) {
		t.Fatalf("edited pre-7e0b788 list changed: %+v, %v", cfg.Redaction.Patterns, err)
	}
	assertLoadOrInitIdempotent(t, path)
}

func TestLoadOrInitLeavesEditedRedactionList(t *testing.T) {
	edited := slices.Clone(redactionDefaultsV04)
	edited[0].Replacement = "[EMAIL]"
	for _, patterns := range [][]Pattern{edited, redactionDefaultsV04[:4], {}} {
		path := initWithPatterns(t, patterns)
		cfg, err := LoadOrInit(path)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cfg.Redaction.Patterns, patterns) {
			t.Fatalf("edited list changed: %+v", cfg.Redaction.Patterns)
		}
		assertLoadOrInitIdempotent(t, path)
	}
}

func assertLoadOrInitIdempotent(t *testing.T, path string) {
	t.Helper()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrInit(path); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("second LoadOrInit rewrote config.yaml:\n%s\n---\n%s", before, after)
	}
}
