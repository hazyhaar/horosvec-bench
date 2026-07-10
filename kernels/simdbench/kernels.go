// Package simdbench compare les noyaux scalaires de horosvec (rabitq.go,
// vamana.go) à des variantes vectorisées via simd/archsimd (Go 1.26,
// GOEXPERIMENT=simd, CGO_ENABLED=0). Bench jetable, lecture seule sur le
// moteur horosvec — aucune modification de /devhoros/horosvec.
//
// Deux noyaux couverts :
//
//  1. Rerank (~2% CPU) : distance L2 carrée fp32 exacte sur les candidats
//     finaux. Trivialement vectorisable — SIMD lane-wise classique.
//  2. Marche greedy (~70% CPU) : accumulation de LUT (gather + add) sur les
//     codes RaBitQ 1-bit — cf. rabitqDistanceLUT dans horosvec/rabitq.go.
//     Ce noyau lit lut[b*256+code] pour chaque octet du code stocké : c'est
//     un GATHER mémoire indexé par la donnée, pas une opération lane-wise.
package simdbench

// L2DistanceSquaredScalar reproduit horosvec.l2DistanceSquared (vamana.go:75) :
// distance L2 carrée fp32->fp64, déroulée 8x. Noyau du RERANK (~2% CPU).
func L2DistanceSquaredScalar(a, b []float32) float64 {
	var sum float64
	n := len(a)
	i := 0
	for ; i+8 <= n; i += 8 {
		d0 := float64(a[i]) - float64(b[i])
		d1 := float64(a[i+1]) - float64(b[i+1])
		d2 := float64(a[i+2]) - float64(b[i+2])
		d3 := float64(a[i+3]) - float64(b[i+3])
		d4 := float64(a[i+4]) - float64(b[i+4])
		d5 := float64(a[i+5]) - float64(b[i+5])
		d6 := float64(a[i+6]) - float64(b[i+6])
		d7 := float64(a[i+7]) - float64(b[i+7])
		sum += d0*d0 + d1*d1 + d2*d2 + d3*d3 + d4*d4 + d5*d5 + d6*d6 + d7*d7
	}
	for ; i < n; i++ {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return sum
}

// RabitqDistanceLUTScalar reproduit horosvec.rabitqDistanceLUT (rabitq.go:140),
// le noyau réellement exécuté à chaque arête visitée pendant la marche
// greedy (~70% CPU) : gather + accumulation sur une table 256 entrées par
// octet de code stocké.
func RabitqDistanceLUTScalar(lut []float64, querySqNorm float64, storedCode []byte, storedSqNorm, storedL1Norm float64) float64 {
	if storedL1Norm == 0 {
		return storedSqNorm
	}
	var signDot float64
	for b, code := range storedCode {
		signDot += lut[b*256+int(code)]
	}
	return querySqNorm + storedSqNorm - 2.0*storedSqNorm*signDot/storedL1Norm
}

// BuildRabitqLUT reproduit horosvec.buildRabitqLUT (rabitq.go:110).
func BuildRabitqLUT(queryCentered []float64, lut []float64) {
	dim := len(queryCentered)
	nBytes := (dim + 7) / 8
	for b := 0; b < nBytes; b++ {
		start := b * 8
		end := start + 8
		if end > dim {
			end = dim
		}
		base := b * 256

		var negSum float64
		for i := start; i < end; i++ {
			negSum -= queryCentered[i]
		}
		lut[base] = negSum

		for p := 1; p < 256; p++ {
			prev := p & (p - 1)
			j := trailingZeros(uint(p))
			if start+j < end {
				lut[base+p] = lut[base+prev] + 2*queryCentered[start+j]
			} else {
				lut[base+p] = lut[base+prev]
			}
		}
	}
}

func trailingZeros(x uint) int {
	n := 0
	for x&1 == 0 {
		x >>= 1
		n++
	}
	return n
}

// RabitqXORPopcountScalar est le noyau SYMÉTRIQUE de comparaison
// (horosvec.rabitqDistance, rabitq.go:161) — PAS le chemin de production
// (celui-ci est asymétrique via LUT), mais le candidat naturel à vectoriser
// si le paquet SIMD exposait un POPCOUNT entier. Conservé ici pour établir
// que même cette variante alternative reste hors de portée de
// simd/archsimd (aucune méthode PopCount dans le paquet, sondé au doc).
func RabitqXORPopcountScalar(queryCode, storedCode []byte) int {
	xorCount := 0
	i := 0
	n := len(queryCode)
	for ; i+8 <= n; i += 8 {
		qWord := uint64(queryCode[i]) | uint64(queryCode[i+1])<<8 |
			uint64(queryCode[i+2])<<16 | uint64(queryCode[i+3])<<24 |
			uint64(queryCode[i+4])<<32 | uint64(queryCode[i+5])<<40 |
			uint64(queryCode[i+6])<<48 | uint64(queryCode[i+7])<<56
		sWord := uint64(storedCode[i]) | uint64(storedCode[i+1])<<8 |
			uint64(storedCode[i+2])<<16 | uint64(storedCode[i+3])<<24 |
			uint64(storedCode[i+4])<<32 | uint64(storedCode[i+5])<<40 |
			uint64(storedCode[i+6])<<48 | uint64(storedCode[i+7])<<56
		xorCount += popcount64(qWord ^ sWord)
	}
	for ; i < n; i++ {
		xorCount += popcount64(uint64(queryCode[i] ^ storedCode[i]))
	}
	return xorCount
}

func popcount64(x uint64) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}
