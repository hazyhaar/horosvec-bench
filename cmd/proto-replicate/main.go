// Throwaway probe (audit 2026-07-10) : la contention du verrou de cache db-blob se
// lève-t-elle si chaque worker a SA PROPRE instance d'index (cache non partagé) ?
// Build une fois sur disque, puis N instances rouvertes sur le même fichier.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hazyhaar/horosvec"
	_ "modernc.org/sqlite"
)

type sliceIter struct {
	vecs [][]float32
	i    int
}

func (s *sliceIter) Next() ([]byte, []float32, bool) {
	if s.i >= len(s.vecs) {
		return nil, nil, false
	}
	v := s.vecs[s.i]
	id := []byte(fmt.Sprintf("%d", s.i))
	s.i++
	return id, v, true
}
func (s *sliceIter) Reset() error { s.i = 0; return nil }

func randVecs(n, dim int, seed int64) [][]float32 {
	r := rand.New(rand.NewSource(seed))
	out := make([][]float32, n)
	for i := range out {
		v := make([]float32, dim)
		for j := range v {
			v[j] = r.Float32()
		}
		out[i] = v
	}
	return out
}

func openIdx(path string, cap int) *horosvec.Index {
	db, _ := sql.Open("sqlite", path)
	cfg := horosvec.DefaultConfig()
	cfg.BruteForceThreshold = 0
	cfg.CacheCapacity = cap
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		panic(err)
	}
	return idx
}

func measure(label string, idxs []*horosvec.Index, queries [][]float32, conc int, dur time.Duration) {
	nq := len(queries)
	for _, idx := range idxs {
		for w := 0; w < 500; w++ {
			idx.Search(context.Background(), queries[w%nq], 10)
		}
	}
	var count int64
	var wg sync.WaitGroup
	stop := time.Now().Add(dur)
	for g := 0; g < conc; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			idx := idxs[g%len(idxs)]
			r := rand.New(rand.NewSource(int64(g)))
			for time.Now().Before(stop) {
				idx.Search(context.Background(), queries[r.Intn(nq)], 10)
				atomic.AddInt64(&count, 1)
			}
		}(g)
	}
	wg.Wait()
	fmt.Printf("[%-28s] instances=%-3d conc=%d qps=%.0f\n",
		label, len(idxs), conc, float64(atomic.LoadInt64(&count))/dur.Seconds())
}

func main() {
	n := flag.Int("n", 60000, "nodes")
	dim := flag.Int("dim", 128, "dim")
	secs := flag.Int("secs", 4, "seconds")
	conc := flag.Int("conc", 32, "concurrency")
	flag.Parse()
	dur := time.Duration(*secs) * time.Second
	nq := 500
	vecs := randVecs(*n, *dim, 1)
	queries := randVecs(nq, *dim, 2)

	// build once to disk
	tmp, _ := os.CreateTemp("", "replicate-*.db")
	path := tmp.Name()
	tmp.Close()
	defer os.Remove(path)
	buildIdx := openIdx(path, *n+10000)
	if err := buildIdx.Build(context.Background(), &sliceIter{vecs: vecs}); err != nil {
		panic(err)
	}
	buildIdx.Close()

	// 1 instance partagée (référence)
	one := []*horosvec.Index{openIdx(path, *n+10000)}
	measure("db-blob 1 instance partagee", one, queries, *conc, dur)
	one[0].Close()

	// N instances (une par worker) — cache NON partagé
	many := make([]*horosvec.Index, *conc)
	for i := range many {
		many[i] = openIdx(path, *n+10000)
	}
	measure("db-blob N instances repliquees", many, queries, *conc, dur)
	for _, idx := range many {
		idx.Close()
	}
}
