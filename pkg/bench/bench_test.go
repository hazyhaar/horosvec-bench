package bench

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/hazyhaar/horosvec-bench/pkg/gt"
	"github.com/hazyhaar/horosvec-bench/pkg/protocol"
)

// fakeEngine est un moteur synthétique déterministe et concurrent-safe : la
// recherche est une lecture pure (renvoie les k plus proches par distance L2
// exacte sur un petit corpus), sans état mutable pendant la mesure. Il sert à
// valider le protocole (séquentiel et concurrent) sans dépendance cgo.
type fakeEngine struct {
	base [][]float32
}

func (e *fakeEngine) Name() string { return "fake" }

func (e *fakeEngine) Build(vecs [][]float32) (float64, float64, error) {
	e.base = vecs
	return 0.001, 1000, nil
}

func (e *fakeEngine) SetParam(int) error { return nil }

func (e *fakeEngine) Search(query []float32, k int) ([]uint64, error) {
	type pair struct {
		id   uint64
		dist float32
	}
	pairs := make([]pair, len(e.base))
	for i, v := range e.base {
		var d float32
		for j := range v {
			diff := v[j] - query[j]
			d += diff * diff
		}
		pairs[i] = pair{id: uint64(i), dist: d}
	}
	// tri par insertion partielle sur k (petit corpus de test)
	for a := 0; a < len(pairs); a++ {
		for b := a + 1; b < len(pairs); b++ {
			if pairs[b].dist < pairs[a].dist {
				pairs[a], pairs[b] = pairs[b], pairs[a]
			}
		}
	}
	if k > len(pairs) {
		k = len(pairs)
	}
	out := make([]uint64, k)
	for i := 0; i < k; i++ {
		out[i] = pairs[i].id
	}
	return out, nil
}

func (e *fakeEngine) Close() error { return nil }

func synthCorpus() ([][]float32, [][]float32) {
	const n, dim = 64, 8
	base := make([][]float32, n)
	for i := range base {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32((i*7+j*13)%97) / 97.0
		}
		base[i] = v
	}
	// requêtes JAMAIS dans la base : décalage constant (protocole existant).
	queries := make([][]float32, 8)
	for i := range queries {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32((i*11+j*3)%89)/89.0 + 0.5
		}
		queries[i] = v
	}
	return base, queries
}

func groundFor(base, queries [][]float32, k int) gt.GroundTruth {
	eng := &fakeEngine{}
	if _, _, err := eng.Build(base); err != nil {
		panic(err)
	}
	neighbors := make([][]int, len(queries))
	for i, q := range queries {
		ids, _ := eng.Search(q, k)
		row := make([]int, len(ids))
		for j, id := range ids {
			row[j] = int(id)
		}
		neighbors[i] = row
	}
	return gt.GroundTruth{K: k, Neighbors: neighbors}
}

// captureStdout exécute fn en redirigeant os.Stdout et retourne les lignes émises.
func captureStdout(t *testing.T, fn func() error) ([]protocol.Result, error) {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w

	var lines []protocol.Result
	var scanErr error
	done := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			var res protocol.Result
			if e := json.Unmarshal(sc.Bytes(), &res); e != nil {
				scanErr = e
				break
			}
			lines = append(lines, res)
		}
		close(done)
	}()

	runErr := fn()
	_ = w.Close()
	<-done
	os.Stdout = orig
	_, _ = io.Copy(io.Discard, r)
	if scanErr != nil {
		return nil, scanErr
	}
	return lines, runErr
}

func TestConcurrentProtocol(t *testing.T) {
	const k = 5
	base, queries := synthCorpus()
	eng := &fakeEngine{}
	ground := groundFor(base, queries, k)

	opt := Options{
		DatasetName: "synth",
		K:           k,
		SweepValues: []int{64, 128},
		Concurrency: []int{1, 2, 8},
		ParamLabel:  func(v int) string { return "p" },
	}

	lines, err := captureStdout(t, func() error {
		return RunWithBuild(eng, base, queries, ground, opt)
	})
	if err != nil {
		t.Fatalf("RunWithBuild: %v", err)
	}

	// P1b : une ligne par (sweep × concurrence).
	want := len(opt.SweepValues) * len(opt.Concurrency)
	if len(lines) != want {
		t.Fatalf("attendu %d lignes, obtenu %d", want, len(lines))
	}

	seen := map[int]bool{}
	for _, l := range lines {
		if l.Concurrency < 1 {
			t.Fatalf("champ concurrency absent/invalide : %d", l.Concurrency)
		}
		if l.QPS <= 0 {
			t.Fatalf("QPS incohérent à concurrence %d : %f", l.Concurrency, l.QPS)
		}
		if l.P50Ms < 0 || l.P99Ms < l.P50Ms {
			t.Fatalf("latences incohérentes conc %d : p50=%f p99=%f", l.Concurrency, l.P50Ms, l.P99Ms)
		}
		if l.RecallMean <= 0 {
			t.Fatalf("recall nul à concurrence %d", l.Concurrency)
		}
		seen[l.Concurrency] = true
	}
	for _, c := range opt.Concurrency {
		if !seen[c] {
			t.Fatalf("niveau de concurrence %d jamais émis", c)
		}
	}
}

// TestConcurrentSemanticsIdentical : le mode concurrent ne change PAS les voisins
// rendus (Search reste déterministe en lecture seule).
func TestConcurrentSemanticsIdentical(t *testing.T) {
	const k = 5
	base, queries := synthCorpus()
	eng := &fakeEngine{}
	if _, _, err := eng.Build(base); err != nil {
		t.Fatalf("build: %v", err)
	}

	// référence séquentielle N=1
	ref := make([][]uint64, len(queries))
	for i, q := range queries {
		got, err := eng.Search(q, k)
		if err != nil {
			t.Fatalf("search ref: %v", err)
		}
		ref[i] = got
	}

	// 8 goroutines interrogent le même moteur en parallèle ; chaque résultat doit
	// être identique à la référence.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failure string
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rep := 0; rep < 50; rep++ {
				for i, q := range queries {
					got, err := eng.Search(q, k)
					if err != nil {
						mu.Lock()
						failure = err.Error()
						mu.Unlock()
						return
					}
					for j := range got {
						if got[j] != ref[i][j] {
							mu.Lock()
							failure = "voisins divergents sous concurrence"
							mu.Unlock()
							return
						}
					}
				}
			}
		}()
	}
	wg.Wait()
	if failure != "" {
		t.Fatalf("sémantique concurrente : %s", failure)
	}
}

func TestParseConcurrency(t *testing.T) {
	got, err := ParseConcurrency("1,8,32")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 3 || got[0] != 1 || got[1] != 8 || got[2] != 32 {
		t.Fatalf("parse inattendu : %v", got)
	}
	if _, err := ParseConcurrency("0"); err == nil {
		t.Fatalf("0 aurait dû être rejeté")
	}
	if _, err := ParseConcurrency("-2"); err == nil {
		t.Fatalf("-2 aurait dû être rejeté")
	}
	if _, err := ParseConcurrency(""); err == nil {
		t.Fatalf("chaîne vide aurait dû être rejetée")
	}
}
