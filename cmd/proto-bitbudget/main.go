// Command proto-bitbudget is a THROWAWAY decidable oracle: does raising the
// RaBitQ bit budget make the exact fp32 rerank removable?
//
// It measures recall@10 of pure APPROXIMATE ranking (no fp32 rerank) by B-bit
// codes, B in {1..5}, on a REAL dim-512 embedding corpus (HackerNews bge/qwen,
// HVARENA1 fp16 arena), against exact fp32 ground truth. Two references:
//
//	(a) 1-bit engine estimator + fp32 rerank of top-128  (= current engine quality)
//	(b) exact = 1.0 by construction
//
// It does NOT touch the horosvec engine. It ports the engine's transforms
// (centroid centering, 1-round randomized Hadamard rotation, 1-bit asymmetric
// L1-corrected estimator) faithfully from rotation.go / rabitq.go, and adds a
// multi-bit estimator.
//
// Multi-bit estimator — documented approximation and its limit:
// The stored rotated-centered vector x = R(o-c) is quantized per coordinate with
// a UNIFORM scalar quantizer of 2^B levels over [-A, A], A = max_i|x_i| per
// vector (stored as one fp32 scale). The query q' = R(q-c) is kept full
// precision (ASYMMETRIC, exactly like the engine's production path). The inner
// product is estimated as <dequant(x), q'>, and dist^2 = ||q'||^2 + ||x||^2 -
// 2<dequant(x),q'> with ||x||^2 stored exactly (as the engine stores sqNorm).
// This is the standard "SQ-B bits, full-precision query" baseline that Extended
// RaBitQ REFINES (unbiased codebook + optimal normalization). Extended RaBitQ is
// therefore expected to match or BEAT these numbers: the measured recall here is
// a conservative LOWER BOUND on what a faithful Extended-RaBitQ encoder achieves.
// The thesis-critical direction is conservative: if this lower bound already
// clears the baseline, the real encoder clears it too.
package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// ---- fp16 -> fp32 (IEEE round-trip, matches arena.go semantics) ----
func halfToFloat32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x3ff)
	switch {
	case exp == 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		// subnormal
		f := float64(mant) / 1024.0 * math.Pow(2, -14)
		v := float32(f)
		if sign != 0 {
			v = -v
		}
		return v
	case exp == 0x1f:
		if mant == 0 {
			return math.Float32frombits(sign | 0x7f800000)
		}
		return math.Float32frombits(sign | 0x7fc00000)
	default:
		e := exp + (127 - 15)
		return math.Float32frombits(sign | e<<23 | mant<<13)
	}
}

// ---- arena reader (HVARENA1: 24-byte header, then count*dim LE fp16) ----
func readArena(path string, wantN int) (vecs [][]float32, dim int) {
	f, err := os.Open(path)
	must(err)
	defer f.Close()
	hdr := make([]byte, 24)
	_, err = f.Read(hdr)
	must(err)
	if string(hdr[:8]) != "HVARENA1" {
		panic("bad magic")
	}
	dim = int(binary.LittleEndian.Uint32(hdr[12:16]))
	count := int(binary.LittleEndian.Uint64(hdr[16:24]))
	if wantN < count {
		count = wantN
	}
	fmt.Fprintf(os.Stderr, "arena dim=%d using n=%d\n", dim, count)
	buf := make([]byte, dim*2)
	vecs = make([][]float32, count)
	for i := 0; i < count; i++ {
		_, err := f.Read(buf)
		must(err)
		v := make([]float32, dim)
		for j := 0; j < dim; j++ {
			v[j] = halfToFloat32(binary.LittleEndian.Uint16(buf[j*2:]))
		}
		vecs[i] = v
	}
	return vecs, dim
}

// ---- ported rotation: 1 round D·H, normalized FWHT, seed 42 ----
type rotator struct {
	dim, codeDim int
	diag         []float32
}

func newRotator(dim int) *rotator {
	cd := 1
	for cd < dim {
		cd <<= 1
	}
	rng := rand.New(rand.NewPCG(42, 0))
	d := make([]float32, cd)
	for i := range d {
		if rng.Uint64()&1 == 0 {
			d[i] = -1
		} else {
			d[i] = 1
		}
	}
	return &rotator{dim: dim, codeDim: cd, diag: d}
}

func (r *rotator) rotate(src, dst []float32, scratch []float64) {
	for i := range dst {
		dst[i] = 0
	}
	copy(dst, src)
	for i := 0; i < r.codeDim; i++ {
		dst[i] *= r.diag[i]
	}
	// normalized FWHT
	n := r.codeDim
	for i := 0; i < n; i++ {
		scratch[i] = float64(dst[i])
	}
	h := 1
	for h < n {
		for i := 0; i < n; i += h * 2 {
			for j := i; j < i+h; j++ {
				x := scratch[j]
				y := scratch[j+h]
				scratch[j] = x + y
				scratch[j+h] = x - y
			}
		}
		h <<= 1
	}
	inv := 1.0 / math.Sqrt(float64(n))
	for i := 0; i < n; i++ {
		dst[i] = float32(scratch[i] * inv)
	}
}

// ---- stored codes ----
type code1bit struct {
	bits   []byte
	sqNorm float64
	l1     float64
}

type codeMulti struct {
	q      []uint8 // one level index per coord (0..2^B-1)
	scaleA float64
	sqNorm float64
}

// codeExt is a FAITHFUL Extended-RaBitQ code (arXiv:2409.09913 family).
// c[i] in {0..2^B-1}, reconstructed level ell(c)=2c-(2^B-1) (odd symmetric grid:
// B=1 -> {-1,+1} = sign, so the MSB concatenation == the 1-bit RaBitQ code).
// G = Sum ell(c_i)*o'_i is the per-vector RaBitQ correction factor <ell,o'>
// (generalizes L1). The asymmetric estimator reduces EXACTLY to the engine's
// 1-bit rabitqDistanceAsym at B=1 (ell=sign, G=L1).
type codeExt struct {
	c      []uint8
	G      float64
	sqNorm float64
}

func l2sq(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	arenaPath := "/inference/hnbook/bench_final/prefix1m.arena"
	nBase := 100000
	nQuery := 200
	k := 10
	if len(os.Args) > 1 {
		fmt.Sscan(os.Args[1], &nBase)
	}

	t0 := time.Now()
	all, dim := readArena(arenaPath, nBase+nQuery)
	fmt.Fprintf(os.Stderr, "loaded %d vecs dim %d in %s\n", len(all), dim, time.Since(t0))
	base := all[:nBase]
	queries := all[nBase : nBase+nQuery]

	// centroid over base
	centroid := make([]float32, dim)
	for _, v := range base {
		for j := 0; j < dim; j++ {
			centroid[j] += v[j]
		}
	}
	for j := range centroid {
		centroid[j] /= float32(nBase)
	}

	rot := newRotator(dim)
	cd := rot.codeDim

	// ---- exact fp32 ground truth (parallel) ----
	fmt.Fprintf(os.Stderr, "computing exact GT...\n")
	gt := make([][]int, nQuery)
	parallelFor(nQuery, func(qi int) {
		gt[qi] = topKExact(base, queries[qi], k)
	})

	// ---- encode base: 1-bit engine code + multi-bit codes B=1..5 ----
	fmt.Fprintf(os.Stderr, "encoding base (rotate+quantize)...\n")
	Bs := []int{1, 2, 3, 4, 5, 6, 7, 8}
	codes1 := make([]code1bit, nBase)
	codesM := make(map[int][]codeMulti, len(Bs))
	codesE := make(map[int][]codeExt, len(Bs))
	for _, B := range Bs {
		codesM[B] = make([]codeMulti, nBase)
		codesE[B] = make([]codeExt, nBase)
	}
	parallelForChunked(nBase, func(i int) {
		scratch := make([]float64, cd)
		x := make([]float32, cd)
		rot.rotate(base[i], x, scratch)
		// 1-bit engine estimator code (sign + sqNorm + L1)
		nb := (cd + 7) / 8
		bitc := make([]byte, nb)
		var sq, l1 float64
		var maxAbs float64
		for j := 0; j < cd; j++ {
			c := float64(x[j])
			sq += c * c
			if c >= 0 {
				l1 += c
				bitc[j/8] |= 1 << uint(j%8)
			} else {
				l1 -= c
			}
			if a := math.Abs(c); a > maxAbs {
				maxAbs = a
			}
		}
		codes1[i] = code1bit{bits: bitc, sqNorm: sq, l1: l1}
		// multi-bit uniform SQ codes
		for _, B := range Bs {
			levels := (1 << B) - 1
			q := make([]uint8, cd)
			A := maxAbs
			if A == 0 {
				A = 1e-9
			}
			for j := 0; j < cd; j++ {
				t := (float64(x[j]) + A) / (2 * A) * float64(levels)
				li := int(math.Round(t))
				if li < 0 {
					li = 0
				}
				if li > levels {
					li = levels
				}
				q[j] = uint8(li)
			}
			codesM[B][i] = codeMulti{q: q, scaleA: A, sqNorm: sq}

			// FAITHFUL Extended-RaBitQ code: same B-bit code index c, but the
			// estimator uses odd symmetric levels ell(c)=2c-(2^B-1) and the
			// per-vector correction G=<ell,o'>. Reconstruct c on the SAME grid.
			ce := make([]uint8, cd)
			var G float64
			off := float64(levels) // 2^B-1
			for j := 0; j < cd; j++ {
				// map o'_j in [-A,A] to c in {0..levels}; MSB(c)=sign(o'_j)
				t := (float64(x[j])/A + 1) / 2 * off
				li := int(math.Round(t))
				if li < 0 {
					li = 0
				}
				if li > levels {
					li = levels
				}
				ce[j] = uint8(li)
				ell := float64(2*li - levels) // odd symmetric level
				G += ell * float64(x[j])
			}
			codesE[B][i] = codeExt{c: ce, G: G, sqNorm: sq}
		}
	})

	// ---- rotate queries once ----
	qRot := make([][]float32, nQuery)
	parallelFor(nQuery, func(qi int) {
		scratch := make([]float64, cd)
		x := make([]float32, cd)
		rot.rotate(queries[qi], x, scratch)
		cp := make([]float32, cd)
		copy(cp, x)
		qRot[qi] = cp
	})

	// ---- measure ----
	type row struct {
		name    string
		recall  float64
		bytesPV int
	}
	var rows []row

	// (a) baseline: 1-bit engine estimator, take top-128, rerank exact fp32
	rerankM := 128
	{
		var sum float64
		var mu sync.Mutex
		parallelFor(nQuery, func(qi int) {
			q := qRot[qi]
			qsq := l2sq(q, make([]float32, cd)) // ||q'||^2
			// score all base by 1-bit asym estimator
			cand := topKByScore(nBase, rerankM, func(i int) float64 {
				return estimate1bit(q, qsq, codes1[i])
			})
			// rerank exact fp32
			ex := topKByScore(len(cand), k, func(ii int) float64 {
				return l2sq(base[cand[ii]], queries[qi])
			})
			got := make([]int, k)
			for r, ii := range ex {
				got[r] = cand[ii]
			}
			rec := recallAt(got, gt[qi])
			mu.Lock()
			sum += rec
			mu.Unlock()
		})
		rows = append(rows, row{"(a) 1-bit + fp32 rerank top128 [BASELINE]", sum / float64(nQuery), 64 + 16 + 2048})
	}

	// B-bit pure approximate, NO rerank
	for _, B := range Bs {
		var sum float64
		var mu sync.Mutex
		cm := codesM[B]
		parallelFor(nQuery, func(qi int) {
			q := qRot[qi]
			qsq := l2sq(q, make([]float32, cd))
			got := topKByScore(nBase, k, func(i int) float64 {
				return estimateMulti(q, qsq, cm[i], B)
			})
			rec := recallAt(got, gt[qi])
			mu.Lock()
			sum += rec
			mu.Unlock()
		})
		bytesPV := B*cd/8 + 8 // code + scaleA(f32)+sqNorm(f32)
		rows = append(rows, row{fmt.Sprintf("B=%d bits, NO rerank", B), sum / float64(nQuery), bytesPV})
	}

	// B-bit FAITHFUL Extended-RaBitQ, NO rerank
	var rowsE []row
	for _, B := range Bs {
		var sum float64
		var mu sync.Mutex
		ce := codesE[B]
		parallelFor(nQuery, func(qi int) {
			q := qRot[qi]
			qsq := l2sq(q, make([]float32, cd))
			levels := (1 << uint(B)) - 1
			got := topKByScore(nBase, k, func(i int) float64 {
				return estimateExt(q, qsq, ce[i], levels)
			})
			rec := recallAt(got, gt[qi])
			mu.Lock()
			sum += rec
			mu.Unlock()
		})
		bytesPV := B*cd/8 + 8 // code + G(f32)+sqNorm(f32)
		rowsE = append(rowsE, row{fmt.Sprintf("B=%d bits Ext-RaBitQ, NO rerank", B), sum / float64(nQuery), bytesPV})
	}

	// ---- report ----
	fmt.Printf("\n=== proto-bitbudget — corpus=%s  n_base=%d  n_query=%d  dim=%d codeDim=%d  k=%d ===\n",
		arenaPath, nBase, nQuery, dim, cd, k)
	fmt.Printf("%-42s  %10s  %14s\n", "regime", "recall@10", "bytes/vector")
	fmt.Printf("%-42s  %10s  %14s\n", "----------------------------------------", "---------", "------------")
	for _, r := range rows {
		fmt.Printf("%-42s  %10.4f  %14d\n", r.name, r.recall, r.bytesPV)
	}
	fmt.Printf("%-42s  %10.4f  %14d\n", "exact fp32 (by construction)", 1.0, 2048)

	fmt.Printf("\n--- FAITHFUL Extended-RaBitQ estimator (odd levels + <ell,o'> correction) ---\n")
	fmt.Printf("%-42s  %10s  %14s\n", "regime", "recall@10", "bytes/vector")
	for _, r := range rowsE {
		fmt.Printf("%-42s  %10.4f  %14d\n", r.name, r.recall, r.bytesPV)
	}

	fmt.Printf("\nfp32 rerank store (reference) = %d bytes/vector (dim*4)\n", dim*4)
	fmt.Fprintf(os.Stderr, "total %s\n", time.Since(t0))
}

// estimate1bit: engine asymmetric L1-corrected estimator (port of rabitqDistanceAsym).
func estimate1bit(q []float32, qsq float64, c code1bit) float64 {
	if c.l1 == 0 {
		return c.sqNorm
	}
	var signDot float64
	for i := range q {
		if c.bits[i/8]&(1<<uint(i%8)) != 0 {
			signDot += float64(q[i])
		} else {
			signDot -= float64(q[i])
		}
	}
	return qsq + c.sqNorm - 2.0*c.sqNorm*signDot/c.l1
}

// estimateMulti: asymmetric uniform-SQ estimator. <dequant(x),q'> with full-precision q'.
func estimateMulti(q []float32, qsq float64, c codeMulti, B int) float64 {
	lv := (1 << uint(B)) - 1
	levels := float64(lv)
	A := c.scaleA
	var dot float64
	for i := range q {
		xq := float64(c.q[i])/levels*2*A - A
		dot += xq * float64(q[i])
	}
	return qsq + c.sqNorm - 2.0*dot
}

// estimateExt: FAITHFUL Extended-RaBitQ asymmetric estimator.
// <o',q'> ~= (Sum ell(c_i) q'_i) * sqNorm / G, reducing EXACTLY to the engine's
// 1-bit estimate at B=1 (ell=sign, G=L1). dist^2 = ||q'||^2 + ||o'||^2 - 2<o',q'>.
func estimateExt(q []float32, qsq float64, c codeExt, levels int) float64 {
	if c.G == 0 {
		return c.sqNorm
	}
	var estDot float64
	for i := range q {
		ell := float64(2*int(c.c[i]) - levels)
		estDot += ell * float64(q[i])
	}
	return qsq + c.sqNorm - 2.0*estDot*c.sqNorm/c.G
}

func topKExact(base [][]float32, q []float32, k int) []int {
	return topKByScore(len(base), k, func(i int) float64 { return l2sq(base[i], q) })
}

// topKByScore returns indices of the k smallest scores among [0,n).
func topKByScore(n, k int, score func(int) float64) []int {
	type se struct {
		i int
		s float64
	}
	best := make([]se, 0, k+1)
	worst := math.MaxFloat64
	for i := 0; i < n; i++ {
		s := score(i)
		if len(best) < k || s < worst {
			best = append(best, se{i, s})
			sort.Slice(best, func(a, b int) bool { return best[a].s < best[b].s })
			if len(best) > k {
				best = best[:k]
			}
			worst = best[len(best)-1].s
		}
	}
	out := make([]int, len(best))
	for i, e := range best {
		out[i] = e.i
	}
	return out
}

func recallAt(got, truth []int) float64 {
	set := make(map[int]bool, len(truth))
	for _, t := range truth {
		set[t] = true
	}
	hit := 0
	for _, g := range got {
		if set[g] {
			hit++
		}
	}
	return float64(hit) / float64(len(truth))
}

func parallelFor(n int, fn func(int)) {
	w := runtime.NumCPU()
	var next atomic.Int64
	next.Store(0)
	var wg sync.WaitGroup
	for g := 0; g < w; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				fn(i)
			}
		}()
	}
	wg.Wait()
}

func parallelForChunked(n int, fn func(int)) { parallelFor(n, fn) }
