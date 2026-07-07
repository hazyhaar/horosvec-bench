package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/hazyhaar/horosvec"
	"github.com/hazyhaar/horosvec-bench/pkg/bench"
	"github.com/hazyhaar/horosvec-bench/pkg/data"
	"github.com/hazyhaar/horosvec-bench/pkg/gt"

	_ "modernc.org/sqlite"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bench-horosvec: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flags := bench.ParseFlags()
	if err := flags.Validate(); err != nil {
		return err
	}

	ds, err := data.Load(flags.Base, flags.Queries, flags.Limit, flags.Holdout)
	if err != nil {
		return err
	}

	ground, err := gt.LoadOrCompute(ds.Base, ds.Queries, flags.K, flags.GT, flags.Base)
	if err != nil {
		return err
	}

	sweep, err := bench.ParseSweep(flags.Sweep)
	if err != nil {
		return err
	}

	eng := &horosvecEngine{}
	defer eng.Close()

	return bench.RunWithBuild(eng, ds.Base, ds.Queries, ground, bench.Options{
		DatasetName: ds.Name,
		K:           flags.K,
		SweepValues: sweep,
		ParamLabel: func(v int) string {
			return strconv.Itoa(v)
		},
	})
}

type horosvecEngine struct {
	idx       *horosvec.Index
	db        *sql.DB
	tmpPath   string
	arenaPath string // non vide quand HOROSVEC_ARENA active le mode arène fp16
}

func (e *horosvecEngine) Name() string {
	if os.Getenv("HOROSVEC_ARENA") != "" {
		return "horosvec-arena"
	}
	return "horosvec"
}

// arenaEnabled indique si le rerank doit lire l'arène fp16 (opt-in via HOROSVEC_ARENA).
func (e *horosvecEngine) arenaEnabled() bool {
	return os.Getenv("HOROSVEC_ARENA") != ""
}

func (e *horosvecEngine) defaultCfg() horosvec.Config {
	cfg := horosvec.DefaultConfig()
	cfg.BruteForceThreshold = 0
	return cfg
}

func (e *horosvecEngine) Build(vecs [][]float32) (float64, float64, error) {
	tmp, err := os.CreateTemp("", "horosvec-bench-*.db")
	if err != nil {
		return 0, 0, fmt.Errorf("temp db: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("close temp: %w", err)
	}

	db, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("open sqlite: %w", err)
	}

	// Mode arène (V2) : poser ArenaPath AVANT Build déclenche le chemin streaming
	// vector-less — l'itérateur est consommé en flux vers l'arène fp16 au fil de l'eau,
	// le graphe est construit depuis l'arène et SQLite est allégé (sans blob vecteur).
	// L'arène sert ensuite le rerank de chaque balayage EfSearch (SetParam la recharge
	// via ArenaPath). Hors mode arène : chemin par défaut strictement inchangé.
	cfg := e.defaultCfg()
	if e.arenaEnabled() {
		e.arenaPath = tmpPath + ".arena"
		cfg.ArenaPath = e.arenaPath
	}

	idx, err := horosvec.New(db, cfg)
	if err != nil {
		_ = db.Close()
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("horosvec new: %w", err)
	}

	iter := &sliceIterator{vecs: vecs}
	t0 := time.Now()
	if err := idx.Build(context.Background(), iter); err != nil {
		_ = db.Close()
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("horosvec build: %w", err)
	}
	elapsed := time.Since(t0).Seconds()

	e.idx = idx
	e.db = db
	e.tmpPath = tmpPath
	return elapsed, float64(len(vecs)) / elapsed, nil
}

func (e *horosvecEngine) SetParam(param int) error {
	if e.db == nil {
		return fmt.Errorf("index non construit")
	}
	cfg := e.defaultCfg()
	cfg.EfSearch = param
	cfg.ArenaPath = e.arenaPath // vide hors mode arène → chemin SQL inchangé
	idx, err := horosvec.New(e.db, cfg)
	if err != nil {
		return fmt.Errorf("horosvec reload ef=%d: %w", param, err)
	}
	e.idx = idx
	return nil
}

func (e *horosvecEngine) Search(query []float32, k int) ([]uint64, error) {
	if e.idx == nil {
		return nil, fmt.Errorf("index non construit")
	}
	results, err := e.idx.Search(context.Background(), query, k)
	if err != nil {
		return nil, err
	}
	ids := make([]uint64, len(results))
	for i, r := range results {
		id, err := strconv.ParseUint(string(r.ID), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse id %q: %w", r.ID, err)
		}
		ids[i] = id
	}
	return ids, nil
}

func (e *horosvecEngine) Close() error {
	if e.db != nil {
		_ = e.db.Close()
	}
	if e.tmpPath != "" {
		_ = os.Remove(e.tmpPath)
	}
	if e.arenaPath != "" {
		_ = os.Remove(e.arenaPath)
	}
	return nil
}

type sliceIterator struct {
	vecs [][]float32
	i    int
}

func (it *sliceIterator) Next() (id []byte, vec []float32, ok bool) {
	if it.i >= len(it.vecs) {
		return nil, nil, false
	}
	id = []byte(strconv.Itoa(it.i))
	vec = it.vecs[it.i]
	it.i++
	return id, vec, true
}

func (it *sliceIterator) Reset() error {
	it.i = 0
	return nil
}
