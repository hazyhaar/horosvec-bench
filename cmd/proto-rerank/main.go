// Throwaway probe (audit 2026-07-10) : isole la BOUCLE DE RERANK des 3 régimes de
// source de vecteurs, la marche greedy (commune, via le plan) étant identique aux 3.
// Chaque "search" = 128 lectures de vecteur candidat + distance L2. Mesure le débit
// de rerank pur à conc 8/16/32 pour 3 sources :
//   1. cache-verrou-fp32  : map sous RWMutex, vec = alloc []float32 dispersée (l'existant)
//   2. contigu-fp32       : []float32 plat, lecture par offset, ZÉRO verrou (flatVecs actuel)
//   3. contigu-fp16       : []uint16 plat, décodage fp16->fp32, zéro verrou (= régime arène)
package main

import (
	"flag"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

const dim = 128
const rerankN = 128 // candidats reclassés par recherche (défaut EfSearch=128)

func l2(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}

// --- source 1 : cache sous RWMutex, vecteurs fp32 dispersés dans le tas ---
type node struct{ vec []float32 }
type lockedCache struct {
	mu    sync.RWMutex
	items map[int64]*node
}

func (c *lockedCache) get(id int64) []float32 {
	c.mu.RLock()
	n := c.items[id]
	c.mu.RUnlock()
	return n.vec
}

func f32toF16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16((b >> 16) & 0x8000)
	exp := int32((b>>23)&0xff) - 127 + 15
	man := (b >> 13) & 0x3ff
	if exp <= 0 {
		return sign
	}
	if exp >= 0x1f {
		return sign | 0x7c00
	}
	return sign | uint16(exp<<10) | uint16(man)
}
func f16toF32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	man := uint32(h & 0x3ff)
	if exp == 0 {
		if man == 0 {
			return math.Float32frombits(sign)
		}
		for man&0x400 == 0 {
			man <<= 1
			exp--
		}
		exp++
		man &= 0x3ff
	} else if exp == 0x1f {
		return math.Float32frombits(sign | 0x7f800000 | man<<13)
	}
	exp = exp - 15 + 127
	return math.Float32frombits(sign | exp<<23 | man<<13)
}

func measure(label string, conc int, dur time.Duration, rerank func(query []float32, ids []int64, buf []float32)) {
	var count int64
	var wg sync.WaitGroup
	stop := time.Now().Add(dur)
	for g := 0; g < conc; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			q := make([]float32, dim)
			for i := range q {
				q[i] = r.Float32()
			}
			ids := make([]int64, rerankN)
			buf := make([]float32, dim)
			for time.Now().Before(stop) {
				for i := range ids {
					ids[i] = int64(r.Intn(60000))
				}
				rerank(q, ids, buf)
				atomic.AddInt64(&count, 1)
			}
		}(g)
	}
	wg.Wait()
	fmt.Printf("[%-22s] conc=%-3d rerank_ops/s=%.0f\n", label, conc, float64(atomic.LoadInt64(&count))/dur.Seconds())
}

func main() {
	n := flag.Int("n", 60000, "nodes")
	secs := flag.Int("secs", 4, "seconds")
	flag.Parse()
	dur := time.Duration(*secs) * time.Second
	r := rand.New(rand.NewSource(1))

	// données : mêmes vecteurs sous 3 représentations
	flat32 := make([]float32, *n*dim)
	flat16 := make([]uint16, *n*dim)
	cache := &lockedCache{items: make(map[int64]*node, *n)}
	for i := 0; i < *n; i++ {
		vec := make([]float32, dim)
		for j := 0; j < dim; j++ {
			v := r.Float32()
			vec[j] = v
			flat32[i*dim+j] = v
			flat16[i*dim+j] = f32toF16(v)
		}
		cache.items[int64(i)] = &node{vec: vec}
	}

	var sink float64
	rerankCache := func(q []float32, ids []int64, buf []float32) {
		var w float64
		for _, id := range ids {
			w += l2(q, cache.get(id))
		}
		sink += w
	}
	rerankFlat32 := func(q []float32, ids []int64, buf []float32) {
		var w float64
		for _, id := range ids {
			w += l2(q, flat32[id*dim:id*dim+dim])
		}
		sink += w
	}
	rerankFlat16 := func(q []float32, ids []int64, buf []float32) {
		var w float64
		for _, id := range ids {
			base := int(id) * dim
			for j := 0; j < dim; j++ {
				buf[j] = f16toF32(flat16[base+j])
			}
			w += l2(q, buf)
		}
		sink += w
	}

	for _, conc := range []int{8, 16, 32} {
		measure("1-cache-verrou-fp32", conc, dur, rerankCache)
		measure("2-contigu-fp32", conc, dur, rerankFlat32)
		measure("3-contigu-fp16", conc, dur, rerankFlat16)
		fmt.Println()
	}
	_ = sink
}
