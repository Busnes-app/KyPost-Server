package sorter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// loadTestModel returns the real model when EMBED_MODEL_DIR points at the
// potion-base-8M files (as in the Docker image), else skips.
func loadTestModel(t *testing.T) *Model {
	t.Helper()
	dir := os.Getenv("EMBED_MODEL_DIR")
	if dir == "" {
		t.Skip("EMBED_MODEL_DIR not set; parity needs the real potion-base-8M files")
	}
	m, err := Load(dir)
	if err != nil {
		t.Fatalf("Load(%s): %v", dir, err)
	}
	return m
}

// The tokenizer and pooling must match the Hugging Face reference exactly:
// a drifting tokenizer does not fail, it just sorts mail worse.
func TestParityWithReferenceTokenizer(t *testing.T) {
	m := loadTestModel(t)
	raw, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Model string `json:"model"`
		Cases []struct {
			Text   string    `json:"text"`
			IDs    []int32   `json:"ids"`
			Vector []float64 `json:"vector"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.Model != ModelID {
		t.Fatalf("fixture is for %s, code pins %s", fx.Model, ModelID)
	}
	for _, c := range fx.Cases {
		got := m.Tokenize(c.Text)
		if len(got) != len(c.IDs) {
			t.Errorf("%q: %d tokens, reference %d\n got %v\nwant %v", clip(c.Text), len(got), len(c.IDs), got, c.IDs)
			continue
		}
		for i := range got {
			if got[i] != c.IDs[i] {
				t.Errorf("%q: token %d = %d, reference %d", clip(c.Text), i, got[i], c.IDs[i])
				break
			}
		}
		v := m.Embed(c.Text)
		var dot float64
		for i := range v {
			dot += float64(v[i]) * c.Vector[i]
		}
		if norm0(c.Vector) && dot < 0.9999 {
			t.Errorf("%q: cosine to reference vector %.6f", clip(c.Text), dot)
		}
	}
}

func norm0(v []float64) bool {
	var s float64
	for _, x := range v {
		s += x * x
	}
	return s > 0
}

func clip(s string) string {
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "..."
	}
	return s
}

// Seeds alone must already sort the obvious cases.
func TestSeedsSortObviousMail(t *testing.T) {
	m := loadTestModel(t)
	labels := []string{"Primary", "Promotions", "Social", "Updates"}
	h, err := Train(t.Context(), m.SeedExamples(labels, nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	for want, text := range map[string]string{
		"Updates":    EmailText("orders@shop.example", "Your order has shipped", "Your package is on its way. Track it."),
		"Promotions": EmailText("deals@store.example", "40% off everything this weekend", "Shop the sale now."),
		"Social":     EmailText("notify@network.example", "Dana wants to connect", "Accept the invitation to join her network."),
		"Primary":    EmailText("sam@friends.example", "Dinner on Friday?", "Are you free Friday? Let me know and I'll book a table."),
	} {
		if got, p := h.Predict(m.Embed(text), labels); got != want {
			t.Errorf("predicted %s (%.2f), want %s", got, p, want)
		}
	}
}

// The head alone, on synthetic clusters: learns a label it has never seen from
// a few corrections, respects the allowlist, and warm-starts to the same answer.
func TestHeadLearnsAndMasks(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	centre := map[string][]float32{"A": unit(rng, 16), "B": unit(rng, 16), "Receipts": unit(rng, 16)}
	sample := func(label string) []float32 {
		v := make([]float32, 16)
		for i := range v {
			v[i] = centre[label][i] + float32(rng.NormFloat64()*0.05)
		}
		return v
	}
	var ex []Example
	for range 10 {
		ex = append(ex, Example{"A", 1, sample("A")}, Example{"B", 1, sample("B")})
	}
	h := mustTrain(t, ex, nil)
	if got, _ := h.Predict(sample("A"), nil); got != "A" {
		t.Fatalf("predicted %s for an A sample", got)
	}
	for range 3 {
		ex = append(ex, Example{"Receipts", 1, sample("Receipts")})
	}
	h2 := mustTrain(t, ex, h)
	if got, _ := h2.Predict(sample("Receipts"), nil); got != "Receipts" {
		t.Fatalf("new label not learned from 3 corrections: got %s", got)
	}
	if got, _ := h2.Predict(sample("Receipts"), []string{"A", "B"}); got == "Receipts" {
		t.Fatal("prediction ignored the allowlist")
	}
	if got, p := h2.Predict(sample("A"), []string{"A"}); got != "A" || math.Abs(p-1) > 1e-9 {
		t.Fatalf("single allowed label: got %s %.3f", got, p)
	}
	if got, _ := mustTrain(t, ex, nil).Predict(sample("B"), nil); got != "B" {
		t.Fatalf("cold start disagrees: %s", got)
	}
	if got, _ := (&Head{}).Predict(sample("A"), nil); got != "" {
		t.Fatal("untrained head must not predict")
	}
}

func mustTrain(t *testing.T, ex []Example, prev *Head) *Head {
	t.Helper()
	h, err := Train(t.Context(), ex, prev)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Training is a cost one account imposes on the shared daemon: over budget it
// refuses, and it stops when its context ends rather than running to completion.
func TestTrainIsBounded(t *testing.T) {
	ex := func(labels, n int) []Example {
		out := make([]Example, n)
		for i := range out {
			out[i] = Example{Label: fmt.Sprint(i % labels), Weight: 1, Vec: []float32{float32(i % labels), 1}}
		}
		return out
	}
	if _, err := Train(t.Context(), ex(MaxLabels+1, MaxLabels+1), nil); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("too many labels: err = %v, want ErrOverBudget", err)
	}
	if _, err := Train(t.Context(), ex(2, MaxExamples+1), nil); !errors.Is(err, ErrOverBudget) {
		t.Fatalf("too many examples: err = %v, want ErrOverBudget", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Train(ctx, ex(2, 10), nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context: err = %v, want context.Canceled", err)
	}
	if _, err := Train(t.Context(), ex(MaxLabels, MaxExamples), nil); err != nil {
		t.Fatalf("at the limits: %v", err)
	}
}

// The label form warns past the limit (frontend SORTER_MAX_LABELS). If the two
// disagree the user is told the sorter is on when it is off, or the reverse.
func TestMaxLabelsMatchesFrontend(t *testing.T) {
	path := filepath.Join("..", "..", "..", "frontend", "src", "settings", "sections", "labelPrefs.ts")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	match := regexp.MustCompile(`const SORTER_MAX_LABELS = ([0-9]+);`).FindSubmatch(source)
	if match == nil {
		t.Fatal("SORTER_MAX_LABELS not found in labelPrefs.ts; if it was renamed, update this test and keep the two in agreement")
	}
	if got, _ := strconv.Atoi(string(match[1])); got != MaxLabels {
		t.Fatalf("labelPrefs.ts SORTER_MAX_LABELS = %d, sorter.MaxLabels = %d", got, MaxLabels)
	}
}

func unit(rng *rand.Rand, d int) []float32 {
	v := make([]float32, d)
	var s float64
	for i := range v {
		x := rng.NormFloat64()
		v[i] = float32(x)
		s += x * x
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(s))
	}
	return v
}
