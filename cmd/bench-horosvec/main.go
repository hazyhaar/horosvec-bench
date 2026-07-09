package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
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

	conc, err := bench.ParseConcurrency(flags.Concurrency)
	if err != nil {
		return err
	}

	eng := &horosvecEngine{}
	defer eng.Close()

	return bench.RunWithBuild(eng, ds.Base, ds.Queries, ground, bench.Options{
		DatasetName: ds.Name,
		K:           flags.K,
		SweepValues: sweep,
		Concurrency: conc,
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
	idsPath   string // fichier d'ids uint64 LE, jumeau de l'arène (mode arène)
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

	// Mode arène (V2) : l'entrée de V2 est une arène fp16 DÉJÀ complète (produite par
	// hnbook-embed), pas un itérateur source. Le banc reproduit exactement ce chemin : il
	// écrit d'abord l'arène et son fichier d'ids (uint64 LE, rang = node_id), puis construit
	// l'index via BuildFromArena — le graphe est bâti en lisant les vecteurs à la demande
	// depuis l'arène mmap, sans jamais matérialiser de tampon fp32 plein. La durée mesurée
	// est celle de BuildFromArena (le build gros-index de V2), l'écriture de l'arène relevant
	// de la phase d'embedding amont. Hors mode arène : chemin par défaut strictement inchangé.
	cfg := e.defaultCfg()
	if e.arenaEnabled() {
		e.arenaPath = tmpPath + ".arena"
		e.idsPath = tmpPath + ".ids"
		cfg.ArenaPath = e.arenaPath
		if err := writeArenaFile(e.arenaPath, vecs); err != nil {
			_ = db.Close()
			_ = os.Remove(tmpPath)
			return 0, 0, fmt.Errorf("write arena: %w", err)
		}
		if err := writeIDsFile(e.idsPath, len(vecs)); err != nil {
			_ = db.Close()
			_ = os.Remove(tmpPath)
			return 0, 0, fmt.Errorf("write ids: %w", err)
		}
	}

	idx, err := horosvec.New(db, cfg)
	if err != nil {
		_ = db.Close()
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("horosvec new: %w", err)
	}

	t0 := time.Now()
	if e.arenaEnabled() {
		err = idx.BuildFromArena(context.Background(), e.arenaPath, e.idsPath)
	} else {
		err = idx.Build(context.Background(), &sliceIterator{vecs: vecs})
	}
	if err != nil {
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

// writeArenaFile écrit une arène fp16 plate (format HVARENA1) à partir des vecteurs de build,
// via l'écrivain public d'arène. Reproduit l'artefact que hnbook-embed produit à l'échelle.
func writeArenaFile(path string, vecs [][]float32) error {
	if len(vecs) == 0 {
		return fmt.Errorf("aucun vecteur")
	}
	w, err := horosvec.NewArenaWriter(path, len(vecs[0]))
	if err != nil {
		return err
	}
	for _, v := range vecs {
		if err := w.WriteVec(v); err != nil {
			w.Abort()
			return err
		}
	}
	return w.Finalize()
}

// writeIDsFile écrit le fichier d'ids jumeau de l'arène : un uint64 little-endian par rang
// (rang = node_id). Le banc SIFT utilise le rang lui-même comme id, cohérent avec l'ext_id
// ASCII décimal restitué par Search.
func writeIDsFile(path string, n int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	bw := bufio.NewWriter(f)
	var buf [8]byte
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint64(buf[:], uint64(i))
		if _, err := bw.Write(buf[:]); err != nil {
			_ = f.Close()
			return err
		}
	}
	if err := bw.Flush(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func (e *horosvecEngine) SetParam(param int) error {
	if e.db == nil {
		return fmt.Errorf("index non construit")
	}
	// Libérer la cartographie mmap de l'index courant avant d'en rouvrir une (le rechargement
	// EfSearch reconstruit un Index frais qui remmap l'arène) — évite d'empiler les mappings.
	if e.idx != nil {
		_ = e.idx.Close()
		e.idx = nil
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
	if e.idx != nil {
		_ = e.idx.Close()
		e.idx = nil
	}
	if e.db != nil {
		_ = e.db.Close()
	}
	if e.tmpPath != "" {
		_ = os.Remove(e.tmpPath)
	}
	if e.arenaPath != "" {
		_ = os.Remove(e.arenaPath)
	}
	if e.idsPath != "" {
		_ = os.Remove(e.idsPath)
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
