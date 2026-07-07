package main

import (
	"database/sql"
	"fmt"
	"os"
	"time"

	sqlite_vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	"github.com/hazyhaar/horosvec-bench/pkg/bench"
	"github.com/hazyhaar/horosvec-bench/pkg/data"
	"github.com/hazyhaar/horosvec-bench/pkg/gt"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bench-sqlitevec: %v\n", err)
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

	eng := &sqlitevecEngine{}
	defer eng.Close()

	// sqlite-vec : recherche exacte, un seul point (sweep ignoré).
	return bench.RunWithBuild(eng, ds.Base, ds.Queries, ground, bench.Options{
		DatasetName: ds.Name,
		K:           flags.K,
		SweepValues: []int{0},
		ParamLabel: func(int) string {
			return "exact"
		},
	})
}

type sqlitevecEngine struct {
	db      *sql.DB
	tmpPath string
	dim     int
}

func (e *sqlitevecEngine) Name() string { return "sqlitevec" }

func (e *sqlitevecEngine) Build(vecs [][]float32) (float64, float64, error) {
	sqlite_vec.Auto()

	tmp, err := os.CreateTemp("", "sqlitevec-bench-*.db")
	if err != nil {
		return 0, 0, fmt.Errorf("temp db: %w", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("close temp: %w", err)
	}

	db, err := sql.Open("sqlite3", tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("open sqlite: %w", err)
	}

	if len(vecs) == 0 {
		_ = db.Close()
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("base vide")
	}
	dim := len(vecs[0])
	ddl := fmt.Sprintf("CREATE VIRTUAL TABLE v USING vec0(embedding float[%d])", dim)
	if _, err := db.Exec(ddl); err != nil {
		_ = db.Close()
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("create virtual table: %w", err)
	}

	t0 := time.Now()
	tx, err := db.Begin()
	if err != nil {
		_ = db.Close()
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	stmt, err := tx.Prepare("INSERT INTO v(rowid, embedding) VALUES (?, ?)")
	if err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("prepare insert: %w", err)
	}
	for i, vec := range vecs {
		blob, err := sqlite_vec.SerializeFloat32(vec)
		if err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			_ = db.Close()
			_ = os.Remove(tmpPath)
			return 0, 0, fmt.Errorf("serialize vec %d: %w", i, err)
		}
		if _, err := stmt.Exec(i, blob); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			_ = db.Close()
			_ = os.Remove(tmpPath)
			return 0, 0, fmt.Errorf("insert vec %d: %w", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		_ = db.Close()
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("close stmt: %w", err)
	}
	if err := tx.Commit(); err != nil {
		_ = db.Close()
		_ = os.Remove(tmpPath)
		return 0, 0, fmt.Errorf("commit: %w", err)
	}
	elapsed := time.Since(t0).Seconds()

	e.db = db
	e.tmpPath = tmpPath
	e.dim = dim
	return elapsed, float64(len(vecs)) / elapsed, nil
}

func (e *sqlitevecEngine) SetParam(int) error { return nil }

func (e *sqlitevecEngine) Search(query []float32, k int) ([]uint64, error) {
	blob, err := sqlite_vec.SerializeFloat32(query)
	if err != nil {
		return nil, fmt.Errorf("serialize query: %w", err)
	}
	rows, err := e.db.Query(
		"SELECT rowid, distance FROM v WHERE embedding MATCH ? ORDER BY distance LIMIT ?",
		blob, k,
	)
	if err != nil {
		return nil, fmt.Errorf("knn query: %w", err)
	}
	defer rows.Close()

	var ids []uint64
	for rows.Next() {
		var id int64
		var dist float64
		if err := rows.Scan(&id, &dist); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		ids = append(ids, uint64(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return ids, nil
}

func (e *sqlitevecEngine) Close() error {
	if e.db != nil {
		_ = e.db.Close()
	}
	if e.tmpPath != "" {
		_ = os.Remove(e.tmpPath)
	}
	return nil
}
