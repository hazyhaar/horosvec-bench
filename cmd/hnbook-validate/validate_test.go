package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/hazyhaar/horosvec"
	_ "modernc.org/sqlite"
)

// sliceIter est un VectorIterator sur des vecteurs en tranche (ext_id = rang en décimal
// ASCII, comme le grave le chemin arène de horosvec).
type sliceIter struct {
	vecs [][]float32
	pos  int
}

func (s *sliceIter) Next() (id []byte, vec []float32, ok bool) {
	if s.pos >= len(s.vecs) {
		return nil, nil, false
	}
	i := s.pos
	s.pos++
	return []byte(strconv.Itoa(i)), s.vecs[i], true
}

func (s *sliceIter) Reset() error { s.pos = 0; return nil }

// buildSmallIndex construit un index horosvec réel (chemin arène) sur n vecteurs unitaires,
// écrit le fichier d'ids (uint64 LE, valeur = rang) et retourne les chemins et les vecteurs.
// Le seuil brute-force par défaut (50 000) garantit que Search opère en force brute EXACTE
// sur ce petit corpus — l'oracle de parité attendu est donc 1,0.
func buildSmallIndex(t *testing.T, n, dim int) (indexPath, arenaPath, idsPath string, vecs [][]float32) {
	t.Helper()
	dir := t.TempDir()
	indexPath = filepath.Join(dir, "index.db")
	arenaPath = filepath.Join(dir, "corpus.arena")
	idsPath = arenaPath + ".ids"

	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := horosvec.DefaultConfig()
	cfg.ArenaPath = arenaPath // chemin arène : SQLite vector-less + arène fp16 sur disque
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(20260709, 7))
	vecs = make([][]float32, n)
	for i := range vecs {
		v := make([]float32, dim)
		var norm float64
		for j := range v {
			v[j] = float32(rng.NormFloat64())
			norm += float64(v[j]) * float64(v[j])
		}
		inv := float32(1.0 / math.Sqrt(norm))
		for j := range v {
			v[j] *= inv
		}
		vecs[i] = v
	}
	if err := idx.Build(context.Background(), &sliceIter{vecs: vecs}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, n*8)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint64(buf[i*8:], uint64(i))
	}
	if err := os.WriteFile(idsPath, buf, 0o644); err != nil {
		t.Fatal(err)
	}
	return indexPath, arenaPath, idsPath, vecs
}

// TestValidateExactParity prouve que, lorsque Search opère en force brute exacte, le
// recouvrement et la vérité force brute rendent 1,0, et que le pipeline latences/rerank/JSON
// fonctionne (rerank_sql_loads nul, pass vrai, verdict sérialisable).
func TestValidateExactParity(t *testing.T) {
	const n, dim, topK = 2000, 64, 10
	indexPath, arenaPath, idsPath, vecs := buildSmallIndex(t, n, dim)

	// Requêtes = 20 vecteurs du corpus (le plus proche exact est le vecteur lui-même).
	const nq = 20
	queries := make([]queryInput, nq)
	for i := 0; i < nq; i++ {
		src := vecs[i*(n/nq)]
		vec := make([]float64, dim)
		for j := range src {
			vec[j] = float64(src[j])
		}
		queries[i] = queryInput{
			QID:    json.RawMessage(strconv.Itoa(i)),
			Text:   "requête " + strconv.Itoa(i),
			Vector: vec,
		}
	}

	v, err := runValidation(context.Background(), runConfig{
		indexPath: indexPath,
		arenaPath: arenaPath,
		idsPath:   idsPath,
		topK:      topK,
		workers:   4,
		queries:   queries,
		report:    newProgress(""),
	})
	if err != nil {
		t.Fatalf("runValidation: %v", err)
	}

	if v.Aggregate.OverlapMoyen != 1.0 {
		t.Fatalf("overlap_moyen = %.4f, attendu 1.0", v.Aggregate.OverlapMoyen)
	}
	if v.Aggregate.RerankSQLLoads != 0 {
		t.Fatalf("rerank_sql_loads = %d, attendu 0", v.Aggregate.RerankSQLLoads)
	}
	if v.Aggregate.NonEmpty != nq {
		t.Fatalf("non_empty = %d, attendu %d", v.Aggregate.NonEmpty, nq)
	}
	if !v.Aggregate.Pass {
		t.Fatalf("pass = false, attendu true (verdict=%+v)", v.Aggregate)
	}
	for i, pq := range v.Queries {
		if len(pq.TopKIDs) != topK {
			t.Fatalf("requête %d : top-K %d != %d", i, len(pq.TopKIDs), topK)
		}
		if pq.Overlap != 1.0 {
			t.Fatalf("requête %d : overlap %.4f != 1.0", i, pq.Overlap)
		}
		if pq.LatenceMS < 0 {
			t.Fatalf("requête %d : latence négative %.3f", i, pq.LatenceMS)
		}
	}
	if v.Aggregate.P50MS < 0 || v.Aggregate.P99MS < 0 {
		t.Fatalf("percentiles négatifs p50=%.3f p99=%.3f", v.Aggregate.P50MS, v.Aggregate.P99MS)
	}

	// Le verdict doit être sérialisable et relisable (pipeline JSON).
	out := filepath.Join(t.TempDir(), "verdict.json")
	if err := writeVerdict(out, v); err != nil {
		t.Fatalf("writeVerdict: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var back verdict
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("relecture verdict: %v", err)
	}
	if !back.Aggregate.Pass || back.Aggregate.OverlapMoyen != 1.0 {
		t.Fatalf("verdict relu incohérent : %+v", back.Aggregate)
	}
}

// TestBruteForceTopKExact vérifie la passe force brute isolément : sur un corpus aléatoire, le
// top-K exact rendu correspond au top-K calculé par un tri de référence O(N log N).
func TestBruteForceTopKExact(t *testing.T) {
	const n, dim, topK = 500, 32, 8
	dir := t.TempDir()
	arenaPath := filepath.Join(dir, "bf.arena")

	w, err := horosvec.NewArenaWriter(arenaPath, dim)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	vecs := make([][]float32, n)
	for i := range vecs {
		v := make([]float32, dim)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
		}
		vecs[i] = v
		if err := w.WriteVec(v); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Finalize(); err != nil {
		t.Fatal(err)
	}

	queries := [][]float32{vecs[0], vecs[100], vecs[499]}
	got, count, err := bruteForceTopK(context.Background(), arenaPath, queries, topK, 3, newProgress(""))
	if err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Fatalf("count = %d, attendu %d", count, n)
	}

	// Référence : décoder l'arène (fp16) via le lecteur puis trier — mêmes octets que la force
	// brute, donc mêmes distances.
	ar, err := horosvec.OpenArenaReader(arenaPath)
	if err != nil {
		t.Fatal(err)
	}
	decoded := make([][]float32, n)
	for i := 0; i < n; i++ {
		buf := make([]float32, dim)
		ar.VecInto(int64(i), buf)
		decoded[i] = buf
	}
	for qi, q := range queries {
		type sr struct {
			rank int64
			d    float64
		}
		all := make([]sr, n)
		for i := 0; i < n; i++ {
			all[i] = sr{rank: int64(i), d: l2sq(q, decoded[i])}
		}
		// Tri stable par (distance, rang) — même ordre total que worse().
		for a := 0; a < len(all); a++ {
			for b := a + 1; b < len(all); b++ {
				if all[b].d < all[a].d || (all[b].d == all[a].d && all[b].rank < all[a].rank) {
					all[a], all[b] = all[b], all[a]
				}
			}
		}
		want := make([]int64, topK)
		for i := 0; i < topK; i++ {
			want[i] = all[i].rank
		}
		if len(got[qi]) != topK {
			t.Fatalf("requête %d : %d rangs != %d", qi, len(got[qi]), topK)
		}
		for i := 0; i < topK; i++ {
			if got[qi][i] != want[i] {
				t.Fatalf("requête %d position %d : rang %d != %d (want=%v got=%v)", qi, i, got[qi][i], want[i], want, got[qi])
			}
		}
	}
}
