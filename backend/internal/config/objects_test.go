package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testS3Secret = "test-secret-value-5b1d"

func setS3(t *testing.T) {
	t.Setenv("KYPOST_S3_ENDPOINT", "https://s3.example.test")
	t.Setenv("KYPOST_S3_BUCKET", "kypost-test")
	t.Setenv("KYPOST_S3_REGION", "")
	t.Setenv("KYPOST_S3_ACCESS_KEY", "test-access")
	t.Setenv("KYPOST_S3_SECRET_KEY", testS3Secret)
	t.Setenv("KYPOST_S3_SECRET_KEY_FILE", "")
}

func clearS3(t *testing.T) {
	for _, k := range S3Vars {
		t.Setenv(k, "")
	}
}

// Single mode refuses every KYPOST_S3_* variable, so it cannot half-configure object storage.
func TestSingleRefusesObjectStorage(t *testing.T) {
	t.Setenv("KYPOST_TENANCY", "")
	t.Setenv("KYPOST_STAFF_ORG_ID", "")
	t.Setenv("KY_CELLS", "")
	for _, k := range S3Vars {
		clearS3(t)
		t.Setenv(k, "x")
		if _, _, err := Tenancy(); err == nil || !strings.Contains(err.Error(), "KYPOST_S3_") {
			t.Errorf("%s in single mode: %v", k, err)
		}
	}
	clearS3(t)
	if _, _, err := Tenancy(); err != nil {
		t.Fatal(err)
	}
}

// Multi mode requires the endpoint, bucket, access key and one secret; no refusal echoes one.
func TestObjectStorage(t *testing.T) {
	setS3(t)
	if _, err := ObjectStorage(); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"KYPOST_S3_ENDPOINT", "KYPOST_S3_BUCKET", "KYPOST_S3_ACCESS_KEY", "KYPOST_S3_SECRET_KEY"} {
		setS3(t)
		t.Setenv(k, "")
		if _, err := ObjectStorage(); err == nil || !strings.Contains(err.Error(), "requires object storage") {
			t.Errorf("without %s: %v", k, err)
		}
	}
	setS3(t)
	t.Setenv("KYPOST_S3_ENDPOINT", "http://user:"+testS3Secret+"@s3.example.test")
	if _, err := ObjectStorage(); err == nil || strings.Contains(err.Error(), testS3Secret) {
		t.Fatalf("plain http endpoint: %v", err)
	}

	file := filepath.Join(t.TempDir(), "s3-secret")
	if err := os.WriteFile(file, []byte(testS3Secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	setS3(t)
	t.Setenv("KYPOST_S3_SECRET_KEY", "")
	t.Setenv("KYPOST_S3_SECRET_KEY_FILE", file)
	if _, err := ObjectStorage(); err != nil {
		t.Fatal("secret file:", err)
	}
	t.Setenv("KYPOST_S3_SECRET_KEY", "another-secret")
	if _, err := ObjectStorage(); err == nil || strings.Contains(err.Error(), testS3Secret) || strings.Contains(err.Error(), "another-secret") {
		t.Fatalf("two secrets that differ: %v", err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KYPOST_S3_SECRET_KEY", "")
	if _, err := ObjectStorage(); err == nil {
		t.Fatal("a world-readable secret file was accepted")
	}
}

// KYPOST_S3_PREFIX is the first segment of every object key, so apps can share a bucket.
// Unset is "kypost"; a set value must be one segment.
func TestObjectPrefix(t *testing.T) {
	t.Setenv("KYPOST_S3_PREFIX", "") // restored after the test
	os.Unsetenv("KYPOST_S3_PREFIX")
	if app, err := ObjectPrefix(); err != nil || app != "kypost" {
		t.Fatalf("default: %q %v", app, err)
	}
	t.Setenv("KYPOST_S3_PREFIX", "post-eu")
	if app, err := ObjectPrefix(); err != nil || app != "post-eu" {
		t.Fatalf("set: %q %v", app, err)
	}
	for _, bad := range []string{"", "/kypost", "kypost/", "ky/post", "..", "ky..post", "KyPost"} {
		t.Setenv("KYPOST_S3_PREFIX", bad)
		if _, err := ObjectPrefix(); err == nil || !strings.Contains(err.Error(), "KYPOST_S3_PREFIX") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}
