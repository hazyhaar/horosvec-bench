// proto-growarena — micro-banc R&D du magasin segmenté growarena (code jetable).
//
// Trois volets :
//  1. bench lecture concurrente par node_id (sweep 1→128 goroutines, ids aléatoires,
//     décodage fp16→fp32 complet), sur TROIS magasins comparables à isopérimètre :
//     - proto   : growarena segmenté (segCap 65536, N/segCap segments)
//     - flat    : growarena monosegement (segCap >= N) ≈ l'arène mmap figée
//     - sqlite  : modernc.org/sqlite, blob par requête préparée (pool database/sql)
//  2. preuve d'append-concurrent : un écrivain unique append en boucle pendant que
//     N lecteurs lisent des ids aléatoires <= count publié, chaque vecteur portant
//     un motif déterministe vérifié à CHAQUE lecture → 0 lecture déchirée tolérée.
//  3. preuve de réouverture : Flush, réouverture, motif intégral revérifié.
package main

import (
	"database/sql"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hazyhaar/horosvec-bench/pkg/growarena"
	_ "modernc.org/sqlite"
)

// motif déterministe EXACT en fp16 : entiers < 2048 (représentables sans arrondi).
func pattern(id int64, i int) float32 {
	return float32((id*7 + int64(i)*13) % 2048)
}

func fillVec(id int64, dim int, dst []float32) {
	for i := 0; i < dim; i++ {
		dst[i] = pattern(id, i)
	}
}

func checkVec(id int64, dim int, got []float32) bool {
	for i := 0; i < dim; i++ {
		if got[i] != pattern(id, i) {
			return false
		}
	}
	return true
}

type readFn func(id int64, dst []float32) bool

// sweepRead mesure les lectures/s à chaque palier de concurrence.
func sweepRead(name string, n int64, dim int, levels []int, dur time.Duration, mk func() readFn) {
	for _, c := range levels {
		var total atomic.Int64
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for w := 0; w < c; w++ {
			wg.Add(1)
			go func(seed int64) {
				defer wg.Done()
				read := mk()
				rng := rand.New(rand.NewSource(seed))
				dst := make([]float32, dim)
				var ops int64
				for {
					select {
					case <-stop:
						total.Add(ops)
						return
					default:
					}
					id := rng.Int63n(n)
					if !read(id, dst) {
						panic(fmt.Sprintf("%s: read failed id=%d", name, id))
					}
					if dst[0] != pattern(id, 0) || dst[dim-1] != pattern(id, dim-1) {
						panic(fmt.Sprintf("%s: WRONG DATA id=%d", name, id))
					}
					ops++
				}
			}(int64(c*1000 + w))
		}
		time.Sleep(dur)
		close(stop)
		wg.Wait()
		qps := float64(total.Load()) / dur.Seconds()
		fmt.Printf(`{"store":%q,"n":%d,"dim":%d,"concurrency":%d,"reads_per_s":%.0f}`+"\n",
			name, n, dim, c, qps)
	}
}

func buildStore(dir string, n int64, dim int, segCap int64) *growarena.Store {
	st, err := growarena.Open(dir, dim, segCap)
	if err != nil {
		panic(err)
	}
	vec := make([]float32, dim)
	for id := int64(0); id < n; id++ {
		fillVec(id, dim, vec)
		if _, err := st.Append(vec); err != nil {
			panic(err)
		}
	}
	if err := st.Flush(); err != nil {
		panic(err)
	}
	return st
}

func buildSQLite(path string, n int64, dim int) *sql.DB {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(OFF)&_pragma=cache_size(-65536)&_pragma=mmap_size(268435456)")
	if err != nil {
		panic(err)
	}
	db.SetMaxOpenConns(256)
	db.SetMaxIdleConns(256)
	if _, err := db.Exec(`CREATE TABLE vecs (id INTEGER PRIMARY KEY, vec BLOB NOT NULL)`); err != nil {
		panic(err)
	}
	tx, err := db.Begin()
	if err != nil {
		panic(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO vecs (id, vec) VALUES (?, ?)`)
	if err != nil {
		panic(err)
	}
	blob := make([]byte, dim*2)
	for id := int64(0); id < n; id++ {
		for i := 0; i < dim; i++ {
			binary.LittleEndian.PutUint16(blob[i*2:], growarena.Float32ToFloat16(pattern(id, i)))
		}
		if _, err := stmt.Exec(id, blob); err != nil {
			panic(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		panic(err)
	}
	return db
}

func sqliteReader(db *sql.DB, dim int) readFn {
	stmt, err := db.Prepare(`SELECT vec FROM vecs WHERE id = ?`)
	if err != nil {
		panic(err)
	}
	var blob []byte
	return func(id int64, dst []float32) bool {
		if err := stmt.QueryRow(id).Scan(&blob); err != nil {
			return false
		}
		if len(blob) != dim*2 {
			return false
		}
		for i := 0; i < dim; i++ {
			dst[i] = growarena.Float16ToFloat32(binary.LittleEndian.Uint16(blob[i*2:]))
		}
		return true
	}
}

// verifyConcurrentAppend : écrivain unique en boucle + nReaders vérifiant le motif.
func verifyConcurrentAppend(dir string, dim int, segCap int64, nReaders int, dur time.Duration) {
	st, err := growarena.Open(dir, dim, segCap)
	if err != nil {
		panic(err)
	}
	var torn, reads, appends atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < nReaders; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(seed))
			dst := make([]float32, dim)
			for {
				select {
				case <-stop:
					return
				default:
				}
				n := st.Count()
				if n == 0 {
					continue
				}
				id := rng.Int63n(n)
				if !st.Get(id, dst) {
					torn.Add(1) // id < count publié DOIT être lisible
					continue
				}
				if !checkVec(id, dim, dst) {
					torn.Add(1)
				}
				reads.Add(1)
			}
		}(int64(w + 1))
	}
	// écrivain unique, flush périodique
	vec := make([]float32, dim)
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		id := st.Count()
		fillVec(id, dim, vec)
		if _, err := st.Append(vec); err != nil {
			panic(err)
		}
		appends.Add(1)
		if id%10000 == 9999 {
			if err := st.Flush(); err != nil {
				panic(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	if err := st.Flush(); err != nil {
		panic(err)
	}
	final := st.Count()
	fmt.Printf(`{"test":"concurrent_append","readers":%d,"appends":%d,"reads":%d,"torn_or_wrong":%d,"final_count":%d}`+"\n",
		nReaders, appends.Load(), reads.Load(), torn.Load(), final)
	if err := st.Close(); err != nil {
		panic(err)
	}
	// réouverture : le préfixe committed doit être intact et conforme au motif
	st2, err := growarena.Open(dir, dim, segCap)
	if err != nil {
		panic(err)
	}
	defer st2.Close()
	n2 := st2.Count()
	dst := make([]float32, dim)
	bad := 0
	for id := int64(0); id < n2; id++ {
		if !st2.Get(id, dst) || !checkVec(id, dim, dst) {
			bad++
		}
	}
	fmt.Printf(`{"test":"reopen","count_before_close":%d,"count_after_reopen":%d,"pattern_mismatches":%d}`+"\n",
		final, n2, bad)
	if bad != 0 || n2 != final {
		os.Exit(1)
	}
}

func main() {
	n := flag.Int64("n", 200000, "vecteurs")
	dim := flag.Int("dim", 128, "dimension")
	segCap := flag.Int64("segcap", 65536, "vecteurs par segment (proto)")
	durSec := flag.Float64("dur", 2.0, "durée de mesure par palier (s)")
	verifSec := flag.Float64("verif", 10.0, "durée du test append-concurrent (s)")
	flag.Parse()
	levels := []int{1, 2, 4, 8, 16, 32, 64, 128}
	dur := time.Duration(*durSec * float64(time.Second))

	scratch, err := os.MkdirTemp("", "growarena-bench-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(scratch)
	fmt.Printf(`{"scratch":%q,"n":%d,"dim":%d,"segcap":%d}`+"\n", scratch, *n, *dim, *segCap)

	// 1. proto segmenté
	proto := buildStore(filepath.Join(scratch, "proto"), *n, *dim, *segCap)
	sweepRead("proto-segmented", *n, *dim, levels, dur, func() readFn { return proto.Get })

	// 2. flat monosegement (≈ arène figée : un seul mmap, offset direct)
	flat := buildStore(filepath.Join(scratch, "flat"), *n, *dim, *n)
	sweepRead("flat-mmap-arena-like", *n, *dim, levels, dur, func() readFn { return flat.Get })

	// 3. sqlite modernc blob-par-lecture
	db := buildSQLite(filepath.Join(scratch, "vecs.db"), *n, *dim)
	sweepRead("sqlite-modernc-blob", *n, *dim, levels, dur, func() readFn { return sqliteReader(db, *dim) })
	db.Close()

	// 4. preuve append-concurrent + crash-safety de réouverture
	verifyConcurrentAppend(filepath.Join(scratch, "verify"), *dim, *segCap, 32,
		time.Duration(*verifSec*float64(time.Second)))

	proto.Close()
	flat.Close()
}
