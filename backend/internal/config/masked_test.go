package config

import (
	"testing"
	"time"
)

func TestRefusesAddress(t *testing.T) {
	now := time.Unix(1000, 0)
	s := UserSettings{MaskedAddresses: []MaskedAddress{
		{Address: "live@x.test", State: "active"},
		{Address: "soon@x.test", State: "active", ExpiresAt: 1001},
		{Address: "gone@x.test", State: "active", ExpiresAt: 1000},
		{Address: "paused@x.test", State: "paused"},
		{Address: "deleted@x.test", State: "deleted"},
	}}
	for address, want := range map[string]bool{"live@x.test": false, "soon@x.test": false, "gone@x.test": true, "paused@x.test": true, "deleted@x.test": true, "plain@x.test": false} {
		if got := s.RefusesAddress(address, false, now); got != want {
			t.Errorf("%s refused %v, want %v", address, got, want)
		}
	}
	if s.RefusesAddress("plain@x.test", true, now) {
		t.Fatal("plus addressing is on by default")
	}
	s.RefusePlusAddresses = true
	if !s.RefusesAddress("plain@x.test", true, now) || s.RefusesAddress("plain@x.test", false, now) {
		t.Fatal("plus refusal")
	}
}
