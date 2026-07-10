package simdbench

import (
	"math/rand"
	"testing"
)

const dim = 512

func randVec(seed int64) []float32 {
	r := rand.New(rand.NewSource(seed))
	v := make([]float32, dim)
	for i := range v {
		v[i] = r.Float32()*2 - 1
	}
	return v
}

// --- Rerank kernel : L2 squared, fp32, dim=512 ---

func BenchmarkL2DistanceSquaredScalar(b *testing.B) {
	a := randVec(1)
	c := randVec(2)
	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink = L2DistanceSquaredScalar(a, c)
	}
	_ = sink
}

// --- Marche kernel : RaBitQ asymmetric LUT distance, dim=512 (64 bytes) ---

func BenchmarkRabitqDistanceLUTScalar(b *testing.B) {
	queryCentered := make([]float64, dim)
	r := rand.New(rand.NewSource(3))
	for i := range queryCentered {
		queryCentered[i] = r.Float64()*2 - 1
	}
	nBytes := (dim + 7) / 8
	lut := make([]float64, nBytes*256)
	BuildRabitqLUT(queryCentered, lut)

	storedCode := make([]byte, nBytes)
	r.Read(storedCode)

	var querySqNorm float64
	for _, v := range queryCentered {
		querySqNorm += v * v
	}
	storedSqNorm := 123.4
	storedL1Norm := 45.6

	b.ResetTimer()
	var sink float64
	for i := 0; i < b.N; i++ {
		sink = RabitqDistanceLUTScalar(lut, querySqNorm, storedCode, storedSqNorm, storedL1Norm)
	}
	_ = sink
}

// Noyau symetrique XOR+popcount (candidat alternatif, hors chemin de
// production) — bench de reference pour situer le cout meme si aucune
// variante SIMD n'est possible faute de PopCount expose par archsimd.
func BenchmarkRabitqXORPopcountScalar(b *testing.B) {
	nBytes := (dim + 7) / 8
	q := make([]byte, nBytes)
	s := make([]byte, nBytes)
	r := rand.New(rand.NewSource(4))
	r.Read(q)
	r.Read(s)

	b.ResetTimer()
	var sink int
	for i := 0; i < b.N; i++ {
		sink = RabitqXORPopcountScalar(q, s)
	}
	_ = sink
}
