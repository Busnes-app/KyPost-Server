package api

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// race runs one completion on each replica at once and counts successes.
func race(complete func(srv *Server) bool, a, b *Server) int {
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, srv := range []*Server{a, b} {
		wg.Go(func() {
			<-start
			if complete(srv) {
				wins.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()
	return int(wins.Load())
}

// AC5: one-time authority issued on A is completed exactly once across A and B.
func TestOneTimeAuthorityIsSpentOnceAcrossReplicas(t *testing.T) {
	a, b := sharedPair(t)
	now := time.Now()
	for round := range 5 {
		// An MFA challenge's TOTP step, and an approved push's finish.
		ch, err := a.mfaChallenges.CreateFenced("user-1", 1)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := b.mfaChallenges.Get(ch.ID); !ok || got.UserID != "user-1" {
			t.Fatalf("B cannot see A's challenge: %+v %v", got, ok)
		}
		if n := race(func(srv *Server) bool { return srv.mfaChallenges.ConsumeTOTPStep(ch.ID, 42) == nil }, a, b); n != 1 {
			t.Fatalf("round %d: TOTP step consumed %d times", round, n)
		}
		push, _ := a.mfaChallenges.CreateFenced("user-1", 1)
		if _, err := b.mfaChallenges.ResolvePushWithMatch(push.ID, "dev", true, push.MatchDigits); err != nil {
			t.Fatal(err)
		}
		if n := race(func(srv *Server) bool {
			_, err := srv.mfaChallenges.ConsumePushApproval(push.ID, push.FinishSecret)
			return err == nil
		}, a, b); n != 1 {
			t.Fatalf("round %d: push approval finished %d times", round, n)
		}

		// A verified SSO step-up grant.
		id := "rea_step" + string(rune('a'+round))
		if err := a.putStepUp(id, ssoStepUp{Session: sessionHash("tok"), Action: "act", Created: now, Expires: now.Add(time.Minute), Started: true, Verified: true}); err != nil {
			t.Fatal(err)
		}
		if n := race(func(srv *Server) bool {
			return srv.editStepUp(id, func(c *ssoStepUp, found bool) (rowEdit, bool) {
				if found && c.Verified && c.Session == sessionHash("tok") {
					return rowDrop, true
				}
				return rowKeep, false
			})
		}, a, b); n != 1 {
			t.Fatalf("round %d: step-up spent %d times", round, n)
		}

		// An SSO sign-in state.
		st := "state-" + string(rune('a'+round))
		a.ssoStates.add(st, now)
		if n := race(func(srv *Server) bool { return srv.ssoStates.take(st, time.Now()) }, a, b); n != 1 {
			t.Fatalf("round %d: SSO state taken %d times", round, n)
		}

		// A single-use pairing nonce.
		nonce := "nonce-" + string(rune('a'+round))
		if n := race(func(srv *Server) bool { return srv.singleUse.consume("pair:"+nonce, time.Minute) }, a, b); n != 1 {
			t.Fatalf("round %d: nonce redeemed %d times", round, n)
		}

		// A pending device approval.
		dev := "device-" + string(rune('a'+round))
		a.pairingMu.Lock()
		err = a.pendingPut(pendingDevice{OwnerID: "user-1", Device: state.NativeDevice{DeviceID: dev}, ExpiresAt: now.Add(time.Minute)})
		a.pairingMu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if n := race(func(srv *Server) bool {
			srv.pairingMu.Lock()
			defer srv.pairingMu.Unlock()
			return srv.pendingEdit(dev, time.Now(), func(v *pendingDevice, found bool) (rowEdit, bool) {
				if !found || v.ApprovedID != "" || v.Approving {
					return rowKeep, false
				}
				v.Approving = true
				return rowWrite, true
			})
		}, a, b); n != 1 {
			t.Fatalf("round %d: pending device claimed %d times", round, n)
		}

		// A mail export grant.
		token := "export-" + string(rune('a'+round))
		if err := a.putExportGrant(token, exportGrant{User: "user-1", Session: sessionHash("tok"), Format: "eml-zip", Expires: now.Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		if n := race(func(srv *Server) bool {
			g, refusal := srv.claimExportGrant(token, "user-1", true, sessionHash("tok"), true)
			if refusal == "" {
				srv.endExport(g.User)
			}
			return refusal == ""
		}, a, b); n != 1 {
			t.Fatalf("round %d: export grant downloaded %d times", round, n)
		}
	}
}
