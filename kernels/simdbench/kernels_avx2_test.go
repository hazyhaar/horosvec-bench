//go:build amd64

package simdbench

import (
	"math"
	"math/rand/v2"
	"testing"
)

func makeLUTFixture(dim int, r *rand.Rand) (lut []float64, code []byte) {
	nBytes := (dim + 7) / 8
	lut = make([]float64, nBytes*256)
	for i := range lut {
		lut[i] = r.Float64()*2 - 1
	}
	code = make([]byte, nBytes)
	for i := range code {
		code[i] = byte(r.IntN(256))
	}
	return lut, code
}

func TestRabitqDistanceLUTAVX2MatchesScalar(t *testing.T) {
	r := rand.New(rand.NewPCG(42, 7))
	for trial := 0; trial < 20; trial++ {
		lut, code := makeLUTFixture(512, r)
		qSq := r.Float64() * 10
		sSq := r.Float64() * 10
		sL1 := r.Float64()*10 + 0.1
		want := RabitqDistanceLUTScalar(lut, qSq, code, sSq, sL1)
		got := RabitqDistanceLUTAVX2(lut, qSq, code, sSq, sL1)
		if math.Abs(want-got) > 1e-9*math.Max(1, math.Abs(want)) {
			t.Fatalf("trial %d: scalar=%v avx2=%v", trial, want, got)
		}
	}
}

func BenchmarkRabitqDistanceLUTAVX2(b *testing.B) {
	r := rand.New(rand.NewPCG(42, 7))
	lut, code := makeLUTFixture(512, r)
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink = RabitqDistanceLUTAVX2(lut, 3.0, code, 2.0, 5.0)
	}
	_ = sink
}
