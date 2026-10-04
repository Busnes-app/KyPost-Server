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
