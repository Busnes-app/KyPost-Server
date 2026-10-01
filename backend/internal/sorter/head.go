package sorter

import (
	"math"
	"slices"
)

// Example is one labelled vector: a shipped seed, a label description, or a
// user correction.
type Example struct {
	Label  string
	Weight float64
	Vec    []float32
}

// Head is a class-balanced multinomial logistic regression over vectors.
type Head struct {
	Labels []string
	w      [][]float64 // [dim][len(Labels)]
	b      []float64
}

const (
	headC     = 10.0 // same objective scale as scikit-learn's C
	headIters = 400
	headLR    = 0.05
)

// Train fits a head. prev (may be nil) warm-starts it when the label set and
// dimension are unchanged, which makes retraining after one correction cheap.
// ponytail: full-batch Adam over every example on each retrain; fine for the
// ~4k vectors the per-label correction cap allows, revisit if that cap grows.
func Train(examples []Example, prev *Head) *Head {
	labels := []string{}
	for _, e := range examples {
		if !slices.Contains(labels, e.Label) {
			labels = append(labels, e.Label)
		}
	}
	slices.Sort(labels)
	if len(labels) < 2 || len(examples) == 0 {
		return &Head{Labels: labels}
	}
	n, d, k := len(examples), len(examples[0].Vec), len(labels)
	idx := make([]int, n)
	w := make([]float64, n)
	perClass := make([]float64, k)
	var total float64
	for i, e := range examples {
		idx[i] = slices.Index(labels, e.Label)
		w[i] = e.Weight
		perClass[idx[i]] += e.Weight
		total += e.Weight
	}
	var wsum float64
	for i := range w {
		w[i] *= total / (float64(k) * perClass[idx[i]])
		wsum += w[i]
	}
	for i := range w {
		w[i] /= wsum
	}

	warm := prev != nil && slices.Equal(prev.Labels, labels) && len(prev.w) == d
	W, B := zeros(d, k), make([]float64, k)
	iters := headIters
	if warm {
		for j := range W {
			copy(W[j], prev.w[j])
		}
		copy(B, prev.b)
		iters /= 4
	}
	l2 := 1 / (headC * float64(n))
	mW, vW, mB, vB := zeros(d, k), zeros(d, k), make([]float64, k), make([]float64, k)
	gW, gB := zeros(d, k), make([]float64, k)
	z := make([]float64, k)
	for t := 1; t <= iters; t++ {
		for j := range gW {
			for c := range gW[j] {
				gW[j][c] = l2 * W[j][c]
			}
		}
		clear(gB)
		for i, e := range examples {
			logits(e.Vec, W, B, z)
			softmax(z)
			z[idx[i]]--
			for c := range z {
				g := z[c] * w[i]
				gB[c] += g
				if g == 0 {
					continue
				}
				for j, x := range e.Vec {
					gW[j][c] += g * float64(x)
				}
			}
		}
		c1, c2 := 1-math.Pow(0.9, float64(t)), 1-math.Pow(0.999, float64(t))
		for j := range W {
			for c := range W[j] {
				adam(&W[j][c], &mW[j][c], &vW[j][c], gW[j][c], c1, c2)
			}
		}
		for c := range B {
			adam(&B[c], &mB[c], &vB[c], gB[c], c1, c2)
		}
	}
	return &Head{Labels: labels, w: W, b: B}
}

func adam(p, m, v *float64, g, c1, c2 float64) {
	*m = 0.9**m + 0.1*g
	*v = 0.999**v + 0.001*g*g
	*p -= headLR * (*m / c1) / (math.Sqrt(*v/c2) + 1e-8)
}

// Predict returns the most likely label among allowed (nil = any) and its
// probability renormalised over the allowed labels. Empty label when fewer
// than one allowed label has examples.
func (h *Head) Predict(vec []float32, allowed []string) (string, float64) {
	if h == nil || h.w == nil {
		return "", 0
	}
	z := make([]float64, len(h.Labels))
	logits(vec, h.w, h.b, z)
	best, bestZ, m := -1, math.Inf(-1), math.Inf(-1)
	for c, l := range h.Labels {
		if allowed != nil && !slices.Contains(allowed, l) {
			z[c] = math.Inf(-1)
			continue
		}
		m = max(m, z[c])
		if z[c] > bestZ {
			best, bestZ = c, z[c]
		}
	}
	if best < 0 {
		return "", 0
	}
	var sum float64
	for c := range z {
		if !math.IsInf(z[c], -1) {
			sum += math.Exp(z[c] - m)
		}
	}
	return h.Labels[best], math.Exp(bestZ-m) / sum
}

func logits(x []float32, W [][]float64, b, out []float64) {
	copy(out, b)
	for j, v := range x {
		if v == 0 {
			continue
		}
		for c, wc := range W[j] {
			out[c] += float64(v) * wc
		}
	}
}

func softmax(z []float64) {
	m := slices.Max(z)
	var s float64
	for i := range z {
		z[i] = math.Exp(z[i] - m)
		s += z[i]
	}
	for i := range z {
		z[i] /= s
	}
}

func zeros(d, k int) [][]float64 {
	out := make([][]float64, d)
	for i := range out {
		out[i] = make([]float64, k)
	}
	return out
}
