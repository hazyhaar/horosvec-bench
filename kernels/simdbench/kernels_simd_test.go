//go:build goexperiment.simd

package simdbench

import (
	"math/rand"
	"testing"
)

func BenchmarkL2DistanceSquaredSIMD(b *testing.B) {
	a := randVec(1)
	c := randVec(2)
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink = L2DistanceSquaredSIMD(a, c)
	}
	_ = sink
}

func TestL2DistanceSquaredSIMDMatchesScalar(t *testing.T) {
	r := rand.New(rand.NewSource(99))
	for trial := 0; trial < 20; trial++ {
		a := make([]float32, dim)
		c := make([]float32, dim)
		for i := range a {
			a[i] = r.Float32()*2 - 1
			c[i] = r.Float32()*2 - 1
		}
		scalar := L2DistanceSquaredScalar(a, c)
		simd := L2DistanceSquaredSIMD(a, c)
		diff := scalar - simd
		if diff < 0 {
			diff = -diff
		}
		if diff > 1e-3 {
			t.Fatalf("mismatch trial %d: scalar=%v simd=%v diff=%v", trial, scalar, simd, diff)
		}
	}
}
