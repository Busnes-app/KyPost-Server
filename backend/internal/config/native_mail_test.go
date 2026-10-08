package config

import "testing"

func TestNativeMailRequiresExplicitBoolean(t *testing.T) {
	for _, value := range []string{"", "false", "true", "yes", "tru"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("KYPOST_NATIVE_MAIL", value)
			enabled, err := NativeMailEnabled()
			valid := value == "" || value == "false" || value == "true"
			if (err == nil) != valid || enabled != (value == "true") {
				t.Fatalf("enabled=%v error=%v", enabled, err)
			}
		})
	}
}

func TestMailboxQuotaBytes(t *testing.T) {
	for value, want := range map[string]int64{"": 5 << 30, "268435456": 256 << 20, "1099511627776": 1 << 40, " 10737418240 ": 10 << 30, "268435455": 0, "1099511627777": 0, "5GiB": 0, "-1": 0} {
		t.Setenv("KYPOST_MAILBOX_QUOTA_BYTES", value)
		got, err := MailboxQuotaBytes()
		if got != want || (err == nil) != (want != 0) {
			t.Fatalf("%q: got %d, %v", value, got, err)
		}
	}
}
