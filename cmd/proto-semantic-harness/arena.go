package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

const (
	arenaMagic      = "HVARENA1"
	arenaHeaderSize = 24
	defaultArenaDim = 512
)

// float16ToFloat32 convertit un fp16 (IEEE 754 half) en float32. Copié depuis horosvec/arena.go.
func float16ToFloat32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x03ff)

	if exp == 0 {
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		exp32 := uint32(127 - 15 + 1)
		for mant&0x0400 == 0 {
			mant <<= 1
			exp32--
		}
		mant &= 0x03ff
		return math.Float32frombits(sign | exp32<<23 | mant<<13)
	}
	if exp == 0x1f {
		if mant == 0 {
			return math.Float32frombits(sign | 0x7f800000)
		}
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	}
	exp32 := exp - 15 + 127
	return math.Float32frombits(sign | exp32<<23 | mant<<13)
}

type arenaReader struct {
	f     *os.File
	dim   int
	count int64
	buf   []byte
	rank  map[int64]int64 // ext_id -> rang dans l'arène (sidecar .ids)
}

func openArena(path string) (*arenaReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	hdr := make([]byte, arenaHeaderSize)
	if _, err := f.Read(hdr); err != nil {
		_ = f.Close()
		return nil, err
	}
	if string(hdr[:8]) != arenaMagic {
		_ = f.Close()
		return nil, fmt.Errorf("arena: magic invalide %q", hdr[:8])
	}
	dim := int(binary.LittleEndian.Uint32(hdr[12:16]))
	count := int64(binary.LittleEndian.Uint64(hdr[16:24]))
	if dim != defaultArenaDim {
		_ = f.Close()
		return nil, fmt.Errorf("arena: dim=%d, attendu %d", dim, defaultArenaDim)
	}
	a := &arenaReader{f: f, dim: dim, count: count, buf: make([]byte, dim*2)}
	// Mapping rang→ext_id : le pipeline d'embedding a SAUTÉ les items morts/vides,
	// le rang n'est PAS id-1 (vérifié au sol 2026-07-12 : rang 3068 → ext_id 3176).
	// Le fichier sidecar .ids (uint64 LE, rang = position) fait foi.
	idsF, err := os.Open(path + ".ids")
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("arena: fichier .ids requis (mapping rang→ext_id): %w", err)
	}
	defer idsF.Close()
	raw, err := io.ReadAll(idsF)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if int64(len(raw)) != count*8 {
		_ = f.Close()
		return nil, fmt.Errorf("arena: .ids taille %d != count*8 (%d)", len(raw), count*8)
	}
	a.rank = make(map[int64]int64, count)
	for r := int64(0); r < count; r++ {
		a.rank[int64(binary.LittleEndian.Uint64(raw[r*8:]))] = r
	}
	return a, nil
}

// errIDAbsent : l'item n'a pas de vecteur dans l'arène (sauté à l'embedding).
var errIDAbsent = errors.New("arena: ext_id absent du mapping .ids")

func (a *arenaReader) Close() error {
	if a == nil || a.f == nil {
		return nil
	}
	return a.f.Close()
}

func (a *arenaReader) Dim() int { return a.dim }

func (a *arenaReader) ReadVec(id int) ([]float32, error) {
	rank, ok := a.rank[int64(id)]
	if !ok {
		return nil, errIDAbsent
	}
	offset := int64(arenaHeaderSize) + rank*int64(a.dim)*2
	if _, err := a.f.Seek(offset, 0); err != nil {
		return nil, err
	}
	if _, err := a.f.Read(a.buf); err != nil {
		return nil, err
	}
	vec := make([]float32, a.dim)
	for j := 0; j < a.dim; j++ {
		h := binary.LittleEndian.Uint16(a.buf[j*2:])
		vec[j] = float16ToFloat32(h)
	}
	return vec, nil
}

func l2Squared(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}
