//go:build goexperiment.simd

package simdbench

import "simd/archsimd"

// L2DistanceSquaredSIMD est la variante vectorisée du noyau de RERANK
// (~2% CPU) via simd/archsimd, lanes Float32x8 (AVX2, 8 float32 par
// registre). Compile uniquement sous GOEXPERIMENT=simd.
func L2DistanceSquaredSIMD(a, b []float32) float64 {
	n := len(a)
	i := 0
	var accum archsimd.Float32x8
	for ; i+8 <= n; i += 8 {
		va := archsimd.LoadFloat32x8Slice(a[i : i+8])
		vb := archsimd.LoadFloat32x8Slice(b[i : i+8])
		d := va.Sub(vb)
		accum = accum.Add(d.Mul(d))
	}
	var lanes [8]float32
	accum.StoreSlice(lanes[:])
	var sum float64
	for _, v := range lanes {
		sum += float64(v)
	}
	for ; i < n; i++ {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return sum
}
