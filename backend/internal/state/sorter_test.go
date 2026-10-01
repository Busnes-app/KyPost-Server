package state

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func newSorterStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func correctionLabels(t *testing.T, s *Store) map[string]int {
	t.Helper()
	ex, err := s.SorterCorrectionsStrict("m1", MaxSorterCorrections)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, e := range ex {
		out[e.Label]++
	}
	return out
}

func TestSorterCorrectionLifecycle(t *testing.T) {
	s := newSorterStore(t)
	check := SorterCheck("Shop <orders@shop.example>", "Your order shipped")
	if err := s.RecordSorterPrediction("42", check, "m1", []float32{0.6, 0.8}, "Updates"); err != nil {
		t.Fatal(err)
	}
	gen0, _ := s.SorterGeneration()

	for _, tc := range []struct {
		name, id, check, label string
		want                   bool
	}{
		{"unknown message teaches nothing", "99", check, "Promotions", false},
		{"mismatched check (reused UID) teaches nothing", "42", SorterCheck("x", "y"), "Promotions", false},
		{"same as prediction teaches nothing", "42", check, "Updates", false},
		{"a real correction is learned", "42", check, "Promotions", true},
		{"repeating it changes nothing", "42", check, "Promotions", false},
		{"relabelling again replaces it", "42", check, "Receipts", true},
	} {
		got, err := s.RecordSorterCorrection(tc.id, tc.check, tc.label)
		if err != nil || got != tc.want {
			t.Fatalf("%s: learned=%v err=%v, want %v", tc.name, got, err, tc.want)
		}
	}
	if got := correctionLabels(t, s); len(got) != 1 || got["Receipts"] != 1 {
		t.Fatalf("one correction per message expected, got %v", got)
	}
	if gen, _ := s.SorterGeneration(); gen == gen0 {
		t.Fatal("generation must change when corrections change")
	}
	ex, _ := s.SorterCorrectionsStrict("m1", MaxSorterCorrections)
	if len(ex) != 1 || ex[0].Vec[0] != 0.6 || ex[0].Vec[1] != 0.8 {
		t.Fatalf("vector not round-tripped: %+v", ex)
	}
	if other, _ := s.SorterCorrectionsStrict("another-model", MaxSorterCorrections); len(other) != 0 {
		t.Fatal("corrections from another embedding model must be ignored")
	}

	// Filing it back under the sorter's own answer withdraws the correction.
	if got, err := s.RecordSorterCorrection("42", check, "Updates"); err != nil || !got {
		t.Fatalf("withdraw: learned=%v err=%v", got, err)
	}
	if got := correctionLabels(t, s); len(got) != 0 {
		t.Fatalf("withdrawn correction still stored: %v", got)
	}
}

// The cap is per account, not per label: spreading corrections over many label
// names must not grow the stored (and trained-on) set.
func TestSorterCorrectionsAreCappedPerAccount(t *testing.T) {
	s := newSorterStore(t)
	for i := range MaxSorterCorrections + 5 {
		id := fmt.Sprint(i)
		_ = s.RecordSorterPrediction(id, "c", "m1", []float32{1}, "Updates")
		if _, err := s.RecordSorterCorrection(id, "c", fmt.Sprint("label-", i%50)); err != nil {
			t.Fatal(err)
		}
	}
	total := 0
	for _, n := range correctionLabels(t, s) {
		total += n
	}
	if total != MaxSorterCorrections {
		t.Fatalf("kept %d corrections, cap is %d", total, MaxSorterCorrections)
	}
	newest, err := s.SorterCorrectionsStrict("m1", 3)
	if err != nil || len(newest) != 3 || newest[0].Label != fmt.Sprint("label-", (MaxSorterCorrections+4)%50) {
		t.Fatalf("limited read = %+v, %v; want the 3 newest, newest first", newest, err)
	}
}

func TestCleanupPrunesSorterPredictionsNotCorrections(t *testing.T) {
	s := newSorterStore(t)
	_ = s.RecordSorterPrediction("1", "c", "m1", []float32{1}, "Updates")
	_, _ = s.RecordSorterCorrection("1", "c", "Promotions")
	_ = s.RecordSorterPrediction("2", "c", "m1", []float32{1}, "Updates")
	old := time.Now().Add(-40 * 24 * time.Hour).Unix()
	if _, err := s.db.Exec(`UPDATE sorter_predictions SET at_unix = ?`, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(30); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM sorter_predictions`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d stale predictions survived cleanup", n)
	}
	if got := correctionLabels(t, s)["Promotions"]; got != 1 {
		t.Fatal("cleanup must not forget what the user taught")
	}
}

// The sorter tables must never hold correspondence text.
func TestSorterTablesHoldNoText(t *testing.T) {
	s := newSorterStore(t)
	for _, table := range []string{"sorter_predictions", "sorter_corrections"} {
		rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var col string
			_ = rows.Scan(&col)
			for _, banned := range []string{"sender", "subject", "body", "snippet"} {
				if strings.Contains(col, banned) {
					t.Errorf("%s.%s looks like correspondence text", table, col)
				}
			}
		}
		_ = rows.Close()
	}
}
