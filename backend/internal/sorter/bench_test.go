package sorter

import (
	"fmt"
	"math/rand/v2"
	"testing"
)

// BenchmarkTrainWorstCase measures a cold train at the largest budget the
// poller allows, which is what one account can make the shared daemon pay.
func BenchmarkTrainWorstCase(b *testing.B) {
	for _, n := range []int{500, MaxExamples} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			rng := rand.New(rand.NewPCG(1, 2))
			ex := make([]Example, n)
			for i := range ex {
				v := make([]float32, 256)
				for j := range v {
					v[j] = float32(rng.NormFloat64())
				}
				ex[i] = Example{Label: fmt.Sprint(i % MaxLabels), Weight: 1, Vec: v}
			}
			b.ResetTimer()
			for range b.N {
				if _, err := Train(b.Context(), ex, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
