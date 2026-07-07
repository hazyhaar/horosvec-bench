package bench

import (
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hazyhaar/horosvec-bench/pkg/gt"
	"github.com/hazyhaar/horosvec-bench/pkg/protocol"
)

const (
	warmupQueries = 20
	minBenchDur   = 3 * time.Second
)

// Engine est l'interface commune des moteurs benchmarkés.
type Engine interface {
	Name() string
	Build(vecs [][]float32) (buildS float64, insertQPS float64, err error)
	SetParam(param int) error
	Search(query []float32, k int) ([]uint64, error)
	Close() error
}

// Options configure une exécution de banc.
type Options struct {
	DatasetName string
	K           int
	SweepValues []int
	ParamLabel  func(int) string
}

// RunWithBuild exécute build + protocole de mesure.
func RunWithBuild(eng Engine, base, queries [][]float32, ground gt.GroundTruth, opt Options) error {
	buildS, insertQPS, err := eng.Build(base)
	if err != nil {
		return fmt.Errorf("bench: build: %w", err)
	}
	return runMeasured(eng, queries, ground, len(base), len(base[0]), opt, buildS, insertQPS)
}

func runMeasured(eng Engine, queries [][]float32, ground gt.GroundTruth, n, dim int, opt Options, buildS, insertQPS float64) error {
	if len(opt.SweepValues) == 0 {
		return fmt.Errorf("bench: aucune valeur de sweep")
	}

	for i := 0; i < warmupQueries && i < len(queries); i++ {
		if _, err := eng.Search(queries[i], opt.K); err != nil {
			return fmt.Errorf("bench: warm-up query %d: %w", i, err)
		}
	}

	for _, param := range opt.SweepValues {
		if err := eng.SetParam(param); err != nil {
			return fmt.Errorf("bench: set param %d: %w", param, err)
		}

		recalls := make([]float64, len(queries))
		for i, q := range queries {
			got, err := eng.Search(q, opt.K)
			if err != nil {
				return fmt.Errorf("bench: recall query %d param %d: %w", i, param, err)
			}
			recalls[i] = gt.Recall(got, ground.Neighbors[i])
		}

		var latencies []float64
		start := time.Now()
		for time.Since(start) < minBenchDur {
			for _, q := range queries {
				t0 := time.Now()
				if _, err := eng.Search(q, opt.K); err != nil {
					return fmt.Errorf("bench: search param %d: %w", param, err)
				}
				latencies = append(latencies, float64(time.Since(t0).Microseconds())/1000.0)
			}
		}
		elapsed := time.Since(start).Seconds()
		qps := float64(len(latencies)) / elapsed

		sort.Float64s(latencies)

		var m runtime.MemStats
		runtime.ReadMemStats(&m)

		paramStr := opt.ParamLabel(param)
		if err := protocol.Emit(protocol.Result{
			Engine:     eng.Name(),
			Dataset:    opt.DatasetName,
			Param:      paramStr,
			N:          n,
			Dim:        dim,
			K:          opt.K,
			BuildS:     buildS,
			InsertQPS:  insertQPS,
			RecallMean: mean(recalls),
			RecallMin:  minFloat(recalls),
			QPS:        qps,
			P50Ms:      percentile(latencies, 0.50),
			P99Ms:      percentile(latencies, 0.99),
			MemMB:      float64(m.HeapAlloc) / (1024 * 1024),
		}); err != nil {
			return err
		}
	}
	return nil
}

// ParseSweep parse une liste d'entiers séparés par des virgules.
func ParseSweep(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("bench: sweep vide")
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		v, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("bench: sweep invalide %q: %w", p, err)
		}
		out = append(out, v)
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

func mean(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	var sum float64
	for _, x := range v {
		sum += x
	}
	return sum / float64(len(v))
}

func minFloat(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	m := v[0]
	for _, x := range v[1:] {
		if x < m {
			m = x
		}
	}
	return m
}
