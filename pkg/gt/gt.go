package gt

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/hazyhaar/horosvec-bench/pkg/data"
)

// GroundTruth contient les k voisins exacts par requête (indices dans la base).
type GroundTruth struct {
	K         int     `json:"k"`
	NBase     int     `json:"n_base"`
	Neighbors [][]int `json:"neighbors"`
}

// LoadOrCompute charge la vérité terrain depuis -gt, le cache .gt.json, ou la calcule.
func LoadOrCompute(base [][]float32, queries [][]float32, k int, gtPath, cacheBase string) (GroundTruth, error) {
	if gtPath != "" {
		return loadFromPath(gtPath, k)
	}
	cachePath := cachePathFor(cacheBase, k)
	if _, err := os.Stat(cachePath); err == nil {
		gt, err := loadJSON(cachePath)
		if err != nil {
			return GroundTruth{}, fmt.Errorf("gt: cache %q: %w", cachePath, err)
		}
		if err := gt.validate(len(queries), len(base), k); err != nil {
			return GroundTruth{}, fmt.Errorf("gt: cache invalide: %w", err)
		}
		return gt, nil
	}

	gt := Compute(base, queries, k)
	if err := saveJSON(cachePath, gt); err != nil {
		return GroundTruth{}, fmt.Errorf("gt: écriture cache %q: %w", cachePath, err)
	}
	return gt, nil
}

func cachePathFor(basePath string, k int) string {
	_ = k
	return basePath + ".gt.json"
}

func loadFromPath(path string, k int) (GroundTruth, error) {
	ext := filepath.Ext(path)
	if ext == ".json" {
		gt, err := loadJSON(path)
		if err != nil {
			return GroundTruth{}, err
		}
		return gt, nil
	}
	neighbors, err := data.LoadIvecs(path)
	if err != nil {
		return GroundTruth{}, fmt.Errorf("gt: ivecs %q: %w", path, err)
	}
	if k > 0 {
		for i, row := range neighbors {
			if len(row) < k {
				return GroundTruth{}, fmt.Errorf("gt: query %d a %d voisins, attendu >= %d", i, len(row), k)
			}
			neighbors[i] = row[:k]
		}
	} else if len(neighbors) > 0 {
		k = len(neighbors[0])
	}
	return GroundTruth{K: k, Neighbors: neighbors}, nil
}

func loadJSON(path string) (GroundTruth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return GroundTruth{}, fmt.Errorf("read: %w", err)
	}
	var gt GroundTruth
	if err := json.Unmarshal(b, &gt); err != nil {
		return GroundTruth{}, fmt.Errorf("unmarshal: %w", err)
	}
	return gt, nil
}

func saveJSON(path string, gt GroundTruth) error {
	b, err := json.Marshal(gt)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

func (g GroundTruth) validate(nQueries, nBase, k int) error {
	if g.K != k {
		return fmt.Errorf("k=%d, attendu %d", g.K, k)
	}
	if g.NBase > 0 && g.NBase != nBase {
		return fmt.Errorf("n_base=%d, attendu %d", g.NBase, nBase)
	}
	if len(g.Neighbors) != nQueries {
		return fmt.Errorf("%d lignes GT, attendu %d requêtes", len(g.Neighbors), nQueries)
	}
	for i, row := range g.Neighbors {
		if len(row) < k {
			return fmt.Errorf("query %d: %d voisins, attendu %d", i, len(row), k)
		}
	}
	return nil
}

// Compute calcule la vérité terrain exacte L2 en parallèle.
func Compute(base [][]float32, queries [][]float32, k int) GroundTruth {
	neighbors := make([][]int, len(queries))
	workers := runtime.GOMAXPROCS(0)
	if workers > len(queries) {
		workers = len(queries)
	}
	if workers < 1 {
		workers = 1
	}

	type job struct {
		qi int
		q  []float32
	}
	jobs := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				neighbors[j.qi] = topKExact(j.q, base, k)
			}
		}()
	}
	for i, q := range queries {
		jobs <- job{qi: i, q: q}
	}
	close(jobs)
	wg.Wait()

	return GroundTruth{
		K:         k,
		NBase:     len(base),
		Neighbors: neighbors,
	}
}

type scored struct {
	id   int
	dist float64
}

func topKExact(query []float32, base [][]float32, k int) []int {
	scores := make([]scored, len(base))
	for i, v := range base {
		scores[i] = scored{id: i, dist: data.L2Distance(query, v)}
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].dist == scores[j].dist {
			return scores[i].id < scores[j].id
		}
		return scores[i].dist < scores[j].dist
	})
	if k > len(scores) {
		k = len(scores)
	}
	out := make([]int, k)
	for i := 0; i < k; i++ {
		out[i] = scores[i].id
	}
	return out
}

// Recall mesure |résultats ∩ GT| / k.
func Recall(got []uint64, want []int) float64 {
	if len(want) == 0 {
		return 0
	}
	k := len(want)
	wantSet := make(map[int]struct{}, k)
	for _, id := range want {
		wantSet[id] = struct{}{}
	}
	hits := 0
	for _, id := range got {
		if _, ok := wantSet[int(id)]; ok {
			hits++
		}
	}
	return float64(hits) / float64(k)
}
