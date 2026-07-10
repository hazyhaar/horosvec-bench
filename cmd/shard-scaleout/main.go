// Command shard-scaleout est un micro-banc autonome qui tranche une question unique :
// la sérialisation des lectures de modernc.org/sqlite vit-elle PAR *sql.DB, ou est-elle
// GLOBALE au process ? Le banc précédent (bench-horosvec, mode db-blob-pool) a prouvé que
// le plateau de débit à ~37-40 kQPS d'une instance SQLite unique n'est PAS un artefact du
// pool database/sql. Reste à savoir si SHARDER en N bases indépendantes DANS LE MÊME
// process lève ce plafond.
//
// Protocole : N fichiers SQLite indépendants (shards), chacun portant une tranche des
// vecteurs dans une table {node_id, vec BLOB}, WAL activé. N *sql.DB distincts ouverts
// dans un seul process. M goroutines lectrices font des SELECT de blob aléatoires,
// réparties sur les N shards (shard et node_id tirés au hasard). Cache chaud (warmup).
// À concurrence M fixe élevée, N varie ; on mesure le débit agrégé.
//
// Lecture du verdict : si le débit dépasse le plafond mono-instance quand N grandit, la
// sérialisation est par-DB et le sharding in-process marche ; s'il reste plat quel que
// soit N, le verrou est global au process et il faut des process séparés.
//
// Ce banc ne dépend PAS de l'index horosvec : il isole la seule primitive en cause, la
// lecture de blob SQLite sous concurrence. Pure Go, CGO off.
package main

import (
	"database/sql"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// dsnPragmas applique les pragmas de performance à CHAQUE connexion du pool (WAL, gros
// cache, mmap) — les mêmes que le mode db-blob-pool de bench-horosvec, pour que le seul
// facteur étudié soit le nombre de shards.
const dsnPragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)" +
	"&_pragma=synchronous(NORMAL)&_pragma=cache_size(-65536)&_pragma=mmap_size(268435456)"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "shard-scaleout: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		nVectors  int
		dim       int
		conc      int
		shardsStr string
		durSec    float64
		poolPer   int
		dir       string
		seed      uint64
	)
	flag.IntVar(&nVectors, "n-vectors", 200000, "nombre total de vecteurs répartis sur les shards")
	flag.IntVar(&dim, "dim", 128, "dimension des vecteurs")
	flag.IntVar(&conc, "conc", 32, "nombre de goroutines lectrices (concurrence fixe)")
	flag.StringVar(&shardsStr, "shards", "1,2,4,8,16,32", "valeurs de N (nombre de shards) balayées")
	flag.Float64Var(&durSec, "dur", 3.0, "durée de la fenêtre de mesure par point (s)")
	flag.IntVar(&poolPer, "pool-per-shard", 64, "MaxOpenConns/MaxIdleConns par *sql.DB de shard")
	flag.StringVar(&dir, "dir", "", "répertoire de travail des shards (requis, doit être writable)")
	flag.Uint64Var(&seed, "seed", 42, "graine du générateur de vecteurs")
	flag.Parse()

	if dir == "" {
		return fmt.Errorf("flag -dir requis")
	}
	shardCounts, err := parseInts(shardsStr)
	if err != nil {
		return err
	}

	fmt.Printf("# shard-scaleout : n_vectors=%d dim=%d conc=%d dur=%.1fs pool_per_shard=%d\n",
		nVectors, dim, conc, durSec, poolPer)
	fmt.Println("# N_shards  qps_aggregate  p50_ms  p99_ms")

	for _, n := range shardCounts {
		res, err := measureN(dir, n, nVectors, dim, conc, poolPer, time.Duration(durSec*float64(time.Second)), seed)
		if err != nil {
			return fmt.Errorf("N=%d: %w", n, err)
		}
		fmt.Printf("%-9d %-14.0f %-7.3f %-7.3f\n", n, res.qps, res.p50, res.p99)
	}
	return nil
}

type point struct {
	qps      float64
	p50, p99 float64
}

// measureN construit N shards portant nVectors répartis, ouvre N *sql.DB, chauffe puis
// mesure le débit agrégé de conc goroutines lectrices sur une fenêtre dur. Les shards sont
// supprimés en sortie.
func measureN(dir string, n, nVectors, dim, conc, poolPer int, dur time.Duration, seed uint64) (point, error) {
	if n < 1 {
		return point{}, fmt.Errorf("N doit être >= 1")
	}
	base := nVectors / n
	if base < 1 {
		return point{}, fmt.Errorf("n_vectors=%d insuffisant pour %d shards", nVectors, n)
	}

	dbs := make([]*sql.DB, n)
	stmts := make([]*sql.Stmt, n)
	paths := make([]string, n)
	cleanup := func() {
		for i := range dbs {
			if stmts[i] != nil {
				_ = stmts[i].Close()
			}
			if dbs[i] != nil {
				_ = dbs[i].Close()
			}
			if paths[i] != "" {
				_ = os.Remove(paths[i])
				_ = os.Remove(paths[i] + "-wal")
				_ = os.Remove(paths[i] + "-shm")
			}
		}
	}
	defer cleanup()

	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	for s := 0; s < n; s++ {
		p := filepath.Join(dir, fmt.Sprintf("shard_%dof%d.db", s, n))
		_ = os.Remove(p)
		paths[s] = p
		db, err := sql.Open("sqlite", p+dsnPragmas)
		if err != nil {
			return point{}, err
		}
		db.SetMaxOpenConns(poolPer)
		db.SetMaxIdleConns(poolPer)
		db.SetConnMaxIdleTime(0)
		db.SetConnMaxLifetime(0)
		dbs[s] = db
		if err := buildShard(db, base, dim, rng); err != nil {
			return point{}, fmt.Errorf("build shard %d: %w", s, err)
		}
		st, err := db.Prepare("SELECT vec FROM vecs WHERE node_id = ?")
		if err != nil {
			return point{}, err
		}
		stmts[s] = st
	}

	readOne := func(r *rand.Rand) error {
		s := r.IntN(n)
		id := r.IntN(base)
		var blob []byte
		if err := stmts[s].QueryRow(id).Scan(&blob); err != nil {
			return err
		}
		if len(blob) != dim*4 {
			return fmt.Errorf("blob de %d octets, attendu %d", len(blob), dim*4)
		}
		return nil
	}

	// Warmup : chaque goroutine chauffe son propre RNG et le page cache.
	{
		var wg sync.WaitGroup
		for w := 0; w < conc; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				r := rand.New(rand.NewPCG(uint64(w)+1, 0xdead))
				for i := 0; i < 500; i++ {
					_ = readOne(r)
				}
			}(w)
		}
		wg.Wait()
	}

	// Fenêtre de mesure : conc goroutines en boucle fermée, latences par worker fusionnées.
	var (
		counter   atomic.Int64
		perWorker = make([][]float64, conc)
		mu        sync.Mutex
		firstErr  error
		wg        sync.WaitGroup
	)
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			r := rand.New(rand.NewPCG(uint64(w)+100, 0xbeef))
			lat := make([]float64, 0, 8192)
			for time.Since(start) < dur {
				t0 := time.Now()
				if err := readOne(r); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				lat = append(lat, float64(time.Since(t0).Microseconds())/1000.0)
				counter.Add(1)
			}
			perWorker[w] = lat
		}(w)
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	if firstErr != nil {
		return point{}, firstErr
	}

	var all []float64
	for _, s := range perWorker {
		all = append(all, s...)
	}
	sort.Float64s(all)
	return point{
		qps: float64(counter.Load()) / elapsed,
		p50: percentile(all, 0.50),
		p99: percentile(all, 0.99),
	}, nil
}

// buildShard crée la table vecs et y insère base vecteurs aléatoires (blob fp32) dans une
// transaction unique. node_id dense 0..base-1.
func buildShard(db *sql.DB, base, dim int, rng *rand.Rand) error {
	if _, err := db.Exec("CREATE TABLE vecs (node_id INTEGER PRIMARY KEY, vec BLOB NOT NULL)"); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	st, err := tx.Prepare("INSERT INTO vecs (node_id, vec) VALUES (?, ?)")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	buf := make([]byte, dim*4)
	for i := 0; i < base; i++ {
		for j := 0; j < dim; j++ {
			binary.LittleEndian.PutUint32(buf[j*4:], math.Float32bits(rng.Float32()))
		}
		if _, err := st.Exec(i, buf); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := st.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func parseInts(s string) ([]int, error) {
	parts := strings.Split(strings.TrimSpace(s), ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		v, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return nil, fmt.Errorf("valeur invalide %q: %w", p, err)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("liste vide")
	}
	return out, nil
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 1 {
		return sorted[len(sorted)-1]
	}
	idx := p * float64(len(sorted)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
