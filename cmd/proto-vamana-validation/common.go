package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"sort"
)

func halfToFloat32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x3ff)
	switch {
	case exp == 0:
		if mant == 0 {
			return math.Float32frombits(sign)
		}
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

// ---- rotation portée (rotation.go : 1 round D·H, graine 42) ----

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

// ---- RaBitQ 1-bit (rabitq.go) ----

func encode1bit(rot []float32, centroid []float32) (code []byte, sqNorm, l1 float64) {
	cd := len(rot)
	nb := (cd + 7) / 8
	code = make([]byte, nb)
	for i := 0; i < cd; i++ {
		c := float64(rot[i]) - float64(centroid[i])
		sqNorm += c * c
		if c >= 0 {
			l1 += c
			code[i/8] |= 1 << uint(i%8)
		} else {
			l1 -= c
		}
	}
	return code, sqNorm, l1
}

func rabitqDistAsym(query []float32, centroid []float32, code []byte, sqNorm, l1 float64) float64 {
	if l1 == 0 {
		return sqNorm
	}
	var signDot, querySq float64
	for i := range query {
		c := float64(query[i]) - float64(centroid[i])
		querySq += c * c
		if code[i/8]&(1<<uint(i%8)) != 0 {
			signDot += c
		} else {
			signDot -= c
		}
	}
	return querySq + sqNorm - 2.0*sqNorm*signDot/l1
}

func rabitqDistPrecomp(qc []float64, querySq float64, code []byte, sqNorm, l1 float64) float64 {
	if l1 == 0 {
		return sqNorm
	}
	var signDot float64
	for i := range qc {
		if code[i/8]&(1<<uint(i%8)) != 0 {
			signDot += qc[i]
		} else {
			signDot -= qc[i]
		}
	}
	return querySq + sqNorm - 2.0*sqNorm*signDot/l1
}

func topKExact(base [][]float32, q []float32, k int) []int {
	return topKByScore(len(base), k, func(i int) float64 { return l2sq(base[i], q) })
}

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

func makeExtID(i int) []byte {
	return []byte(fmt.Sprintf("%08d", i))
}
