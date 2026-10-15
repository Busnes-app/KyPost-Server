package api

import (
	"context"
	"testing"
	"time"
)

func TestSendAsVerificationCooldownBlocksWithinWindow(t *testing.T) {
	c := newCooldown(sendAsVerificationCooldownFor)
	const key = "user-1|alice@example.com"

	ok, retryAfter := c.allow(key)
	if !ok {
		t.Fatal("expected first verification probe to be allowed")
	}
	if retryAfter != 0 {
		t.Fatalf("retryAfter = %v, want 0 when allowed", retryAfter)
	}

	ok, retryAfter = c.allow(key)
	if ok {
		t.Fatal("expected a second probe within the cooldown window to be blocked")
	}
	if retryAfter <= 0 || retryAfter > sendAsVerificationCooldownFor+time.Millisecond {
		t.Fatalf("retryAfter = %v, want a positive duration <= %v (+1ms rounding)", retryAfter, sendAsVerificationCooldownFor)
	}
}

// TestSendAsVerificationCooldownIsPerKey demonstrates the reason this type is
// keyed on userID+"|"+email rather than bare userID like mfaPushCooldown: the
// same user verifying a second alias must not be blocked by the cooldown
// already in effect for their first alias.
func TestSendAsVerificationCooldownIsPerKey(t *testing.T) {
	c := newCooldown(sendAsVerificationCooldownFor)
	const aliceKey = "user-a|alice@example.com"
	const bobKey = "user-a|bob@example.com"

	_, _ = c.allow(aliceKey)

	if ok, _ := c.allow(aliceKey); ok {
		t.Fatal("user-a's probe to alice@example.com should be in cooldown")
	}
	if ok, _ := c.allow(bobKey); !ok {
		t.Fatal("user-a's probe to a different candidate address (bob@example.com) must not be affected by the cooldown on alice@example.com")
	}
}

func TestSendAsVerificationCooldownExpiresAfterWindow(t *testing.T) {
	c := newCooldown(sendAsVerificationCooldownFor)
	now, advance := fixedClock(time.Now())
	c.now = now
	const key = "user-2|carol@example.com"
	_, _ = c.allow(key)
	advance(sendAsVerificationCooldownFor + time.Second)

	if ok, _ := c.allow(key); !ok {
		t.Fatal("expected probe to be allowed again once the cooldown window has elapsed")
	}
}

// TestSendAsVerificationCooldownSweepRemovesStaleEntries proves sweep bounds
// the map's growth: an expired cooldown is removed, a live one is kept.
// Without this, the map grows forever since it's keyed on
// attacker-influenced (userID, candidate-email) pairs that anyone can mint by
// attempting to verify an arbitrary address.
func TestSendAsVerificationCooldownSweepRemovesStaleEntries(t *testing.T) {
	c := newCooldown(sendAsVerificationCooldownFor)
	now, advance := fixedClock(time.Now())
	c.now = now
	const staleKey = "user-1|stale@example.com"
	const freshKey = "user-1|fresh@example.com"

	_, _ = c.allow(staleKey)
	advance(sendAsVerificationCooldownFor + time.Second)
	_, _ = c.allow(freshKey)

	c.sweep()

	_, staleStillPresent := c.entries[staleKey]
	_, freshStillPresent := c.entries[freshKey]
	if staleStillPresent {
		t.Fatal("expected sweep to remove an expired cooldown")
	}
	if !freshStillPresent {
		t.Fatal("expected sweep to keep a cooldown still in effect")
	}
}

// TestStartCooldownSweeperRunsOnTickerAndStopsOnCancel proves the
// background sweeper actually fires sweep() on its ticker and returns
// promptly once its context is canceled, mirroring StartPickupSweeper's
// ticker/select shape (server.go).
func TestStartCooldownSweeperRunsOnTickerAndStopsOnCancel(t *testing.T) {
	srv := newTestServer(t)

	const staleKey = "user-1|stale@example.com"
	_, _ = srv.sendAsCooldown.allow(staleKey)
	_, _ = srv.mfaPushLimiter.allow("user-1")
	// Set before the sweeper starts, so the goroutine only ever reads them.
	later := func() time.Time { return time.Now().Add(2 * time.Hour) }
	srv.sendAsCooldown.now, srv.mfaPushLimiter.now = later, later

	old := cooldownSweepInterval
	cooldownSweepInterval = 10 * time.Millisecond
	t.Cleanup(func() { cooldownSweepInterval = old })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		srv.StartCooldownSweeper(ctx)
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		srv.sendAsCooldown.mu.Lock()
		_, present := srv.sendAsCooldown.entries[staleKey]
		srv.sendAsCooldown.mu.Unlock()
		srv.mfaPushLimiter.mu.Lock()
		_, mfaPresent := srv.mfaPushLimiter.entries["user-1"]
		srv.mfaPushLimiter.mu.Unlock()
		if !present && !mfaPresent {
			break
		}
		select {
		case <-deadline:
			t.Fatal("expected StartCooldownSweeper to have swept the cooldown and the MFA push entry via its ticker")
		case <-time.After(5 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("expected StartCooldownSweeper to return promptly after context cancellation")
	}
}

// The device-index rescan gate admits one rebuild per interval, process-wide.
func TestDeviceRescanGateOncePerInterval(t *testing.T) {
	srv := newTestServer(t)
	now, advance := fixedClock(time.Now())
	srv.deviceRescan.now = now
	if ok, _ := srv.deviceRescan.allow(""); !ok {
		t.Fatal("first rescan refused")
	}
	advance(deviceRescanInterval - time.Second)
	if ok, _ := srv.deviceRescan.allow(""); ok {
		t.Fatal("second rescan inside the interval admitted")
	}
	advance(2 * time.Second)
	if ok, _ := srv.deviceRescan.allow(""); !ok {
		t.Fatal("rescan refused after the interval")
	}
}
