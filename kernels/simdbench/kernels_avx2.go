//go:build amd64

package simdbench

// lutGatherSumAVX2 est le cœur gather du noyau LUT en assembleur AVX2
// (kernels_avx2.s). nBytes doit être un multiple de 4 ; la table lut doit
// couvrir nBytes*256 entrées. Prototype de banc UNIQUEMENT (P2 option A) —
// aucune détection de capacité CPU : à n'exécuter que sur une machine AVX2.
//
//go:noescape
func lutGatherSumAVX2(lut *float64, code *byte, nBytes int) float64

// RabitqDistanceLUTAVX2 : équivalent de RabitqDistanceLUTScalar dont la boucle
// de gather est portée en assembleur AVX2 (VGATHERQPD).
func RabitqDistanceLUTAVX2(lut []float64, querySqNorm float64, storedCode []byte, storedSqNorm, storedL1Norm float64) float64 {
	if storedL1Norm == 0 {
		return storedSqNorm
	}
	signDot := lutGatherSumAVX2(&lut[0], &storedCode[0], len(storedCode))
	return querySqNorm + storedSqNorm - 2.0*storedSqNorm*signDot/storedL1Norm
}
