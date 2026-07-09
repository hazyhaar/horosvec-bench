package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/evan176/hnswgo"
	"github.com/hazyhaar/horosvec-bench/pkg/bench"
	"github.com/hazyhaar/horosvec-bench/pkg/data"
	"github.com/hazyhaar/horosvec-bench/pkg/gt"
)

const (
	hnswM           = 16
	hnswEfConstruct = 200
	hnswSeed        = 42
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bench-hnsw: %v\n", err)
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

	eng := &hnswEngine{ef: sweep[0]}
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

type hnswEngine struct {
	idx *hnswgo.HNSW
	ef  int
}

func (e *hnswEngine) Name() string { return "hnsw" }

func (e *hnswEngine) Build(vecs [][]float32) (float64, float64, error) {
	if len(vecs) == 0 {
		return 0, 0, fmt.Errorf("base vide")
	}
	dim := len(vecs[0])
	maxElem := uint64(len(vecs)) + 1000

	t0 := time.Now()
	idx := hnswgo.New(dim, hnswM, hnswEfConstruct, hnswSeed, maxElem, "l2")
	for i, vec := range vecs {
		idx.AddPoint(vec, uint64(i))
	}
	elapsed := time.Since(t0).Seconds()

	e.idx = idx
	return elapsed, float64(len(vecs)) / elapsed, nil
}

func (e *hnswEngine) SetParam(param int) error {
	if e.idx == nil {
		return fmt.Errorf("index non construit")
	}
	e.ef = param
	e.idx.SetEf(param)
	return nil
}

func (e *hnswEngine) Search(query []float32, k int) ([]uint64, error) {
	if e.idx == nil {
		return nil, fmt.Errorf("index non construit")
	}
	labels, _ := e.idx.SearchKNN(query, k)
	return labels, nil
}

func (e *hnswEngine) Close() error {
	if e.idx != nil {
		e.idx.Free()
		e.idx = nil
	}
	return nil
}
