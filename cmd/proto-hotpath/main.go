// Throwaway probe (audit 2026-07-10 hotpath db-blob). Compares rerank paths
// (arena memory vs db-blob cache vs db-blob SQL-miss) on the SAME corpus, at
// conc 1 and 32, to locate the concurrency divergence. Delete after audit.
package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"runtime/pprof"
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

func (s *sliceIter) Next() (id []byte, vec []float32, ok bool) {
	if s.i >= len(s.vecs) {
		return nil, nil, false
	}
	v := s.vecs[s.i]
	id = []byte(fmt.Sprintf("%d", s.i))
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

func writeArena(path string, vecs [][]float32) {
	w, err := horosvec.NewArenaWriter(path, len(vecs[0]))
	if err != nil {
		panic(err)
	}
	for _, v := range vecs {
		if err := w.WriteVec(v); err != nil {
			panic(err)
		}
	}
	if err := w.Finalize(); err != nil {
		panic(err)
	}
}

func writeIDs(path string, n int) {
	f, _ := os.Create(path)
	var buf [8]byte
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		f.Write(buf[:])
	}
	f.Close()
}

func build(mode string, cacheCap, n, dim int, vecs [][]float32) (*horosvec.Index, func()) {
	tmp, _ := os.CreateTemp("", "hotpath-*.db")
	path := tmp.Name()
	tmp.Close()
	db, _ := sql.Open("sqlite", path)
	cfg := horosvec.DefaultConfig()
	cfg.BruteForceThreshold = 0
	cfg.CacheCapacity = cacheCap
	var arenaPath, idsPath string
	if mode == "arena" {
		arenaPath = path + ".arena"
		idsPath = path + ".ids"
		writeArena(arenaPath, vecs)
		writeIDs(idsPath, n)
		cfg.ArenaPath = arenaPath
	}
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		panic(err)
	}
	if mode == "arena" {
		err = idx.BuildFromArena(context.Background(), arenaPath, idsPath)
	} else {
		err = idx.Build(context.Background(), &sliceIter{vecs: vecs})
	}
	if err != nil {
		panic(err)
	}
	cleanup := func() {
		idx.Close()
		db.Close()
		os.Remove(path)
		if arenaPath != "" {
			os.Remove(arenaPath)
			os.Remove(idsPath)
		}
	}
	return idx, cleanup
}

func measure(label string, idx *horosvec.Index, queries [][]float32, conc int, dur time.Duration, prof string) {
	nq := len(queries)
	for w := 0; w < 2000; w++ {
		idx.Search(context.Background(), queries[w%nq], 10)
	}
	if prof != "" {
		f, _ := os.Create(prof)
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	var count int64
	var wg sync.WaitGroup
	stop := time.Now().Add(dur)
	for g := 0; g < conc; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			r := rand.New(rand.NewSource(int64(g)))
			for time.Now().Before(stop) {
				idx.Search(context.Background(), queries[r.Intn(nq)], 10)
				atomic.AddInt64(&count, 1)
			}
		}(g)
	}
	wg.Wait()
	nSearch := atomic.LoadInt64(&count)
	fmt.Printf("[%-18s] conc=%-3d qps=%.0f\n", label, conc, float64(nSearch)/dur.Seconds())
}

func main() {
	n := flag.Int("n", 60000, "nodes")
	dim := flag.Int("dim", 128, "dim")
	secs := flag.Int("secs", 4, "measure seconds")
	flag.Parse()
	dur := time.Duration(*secs) * time.Second
	nq := 500
	vecs := randVecs(*n, *dim, 1)
	queries := randVecs(nq, *dim, 2)

	prof := "/tmp/claude-1000/-devhoros/9fa20a03-0e83-420f-b497-6458930dea2d/scratchpad/"

	arena, ca := build("arena", *n+10000, *n, *dim, vecs)
	measure("arena", arena, queries, 1, dur, "")
	measure("arena", arena, queries, 32, dur, prof+"cpu_arena.prof")
	ca()

	full, cf := build("db-blob", *n+10000, *n, *dim, vecs)
	measure("dbblob-allcache", full, queries, 1, dur, "")
	measure("dbblob-allcache", full, queries, 32, dur, prof+"cpu_dbblob.prof")
	cf()

	half, ch := build("db-blob", *n/2, *n, *dim, vecs)
	measure("dbblob-halfcache", half, queries, 1, dur, "")
	measure("dbblob-halfcache", half, queries, 32, dur, "")
	ch()
}
