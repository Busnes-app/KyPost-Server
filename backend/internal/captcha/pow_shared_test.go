package captcha

import (
	"context"
	"testing"

	"github.com/Busnes-app/kypost-server/backend/internal/state"
)

// AC8: a proof-of-work solution is spent once across two replicas sharing the database.
func TestPoWSolutionIsSpentOnceAcrossReplicas(t *testing.T) {
	dir := t.TempDir()
	var verifiers [2]*PoWVerifier
	for i := range verifiers {
		st, err := state.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		v, err := NewPoWVerifier([]byte("a shared key of at least thirty-two bytes"), 1000)
		if err != nil {
			t.Fatal(err)
		}
		v.SetSpender(st.SpendChallenge)
		verifiers[i] = v
	}
	ch, err := verifiers[0].IssueAt("203.0.113.9", 1000)
	if err != nil {
		t.Fatal(err)
	}
	token := solve(t, ch)
	if ok, err := verifiers[0].Verify(context.Background(), token, "203.0.113.9"); !ok || err != nil {
		t.Fatalf("first spend: %v %v", ok, err)
	}
	if ok, _ := verifiers[1].Verify(context.Background(), token, "203.0.113.9"); ok {
		t.Fatal("the solution spent on A verified again on B")
	}
}
