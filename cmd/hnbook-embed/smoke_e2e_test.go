package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strconv"
	"testing"

	"github.com/hazyhaar/horosvec"
	_ "modernc.org/sqlite"
)

// arenaIterator alimente horosvec.Build à partir d'une arène fp16 finalisée + son fichier
// d'ids (rang → id HN). ext_id = id HN en décimal ; le vecteur est décodé fp16→fp32.
type arenaIterator struct {
	ar  *horosvec.ArenaReader
	ids []uint64
	i   int64
	dim int
}

func (it *arenaIterator) Next() (id []byte, vec []float32, ok bool) {
	if it.i >= it.ar.Count() {
		return nil, nil, false
	}
	dst := make([]float32, it.dim)
	if !it.ar.VecInto(it.i, dst) {
		return nil, nil, false
	}
	extID := []byte(strconv.FormatUint(it.ids[it.i], 10))
	it.i++
	return extID, dst, true
}

func (it *arenaIterator) Reset() error { it.i = 0; return nil }

// TestSmokeE2E est le smoke C3 : arène produite par hnbook-embed → Build streaming horosvec
// (ArenaPath) → Search top-10 non vide sur 20 requêtes réelles → RerankSQLLoads()==0.
// Env-gated (n'entre pas dans le gate hors-ligne) :
//
//	HNBOOK_SMOKE_ARENA   chemin de l'arène finalisée (.ids attendu à coté)
//	HNBOOK_SMOKE_QUERIES NDJSON {id,text} des requêtes (au moins 20)
//	HNBOOK_ENDPOINT      endpoint /v1/embeddings (défaut :8001)
//	HNBOOK_MODEL/HNBOOK_DIMS  modèle et dimensions (défaut qwen3-embedding-0.6b / 512)
func TestSmokeE2E(t *testing.T) {
	arenaPath := os.Getenv("HNBOOK_SMOKE_ARENA")
	queriesPath := os.Getenv("HNBOOK_SMOKE_QUERIES")
	if arenaPath == "" || queriesPath == "" {
		t.Skip("HNBOOK_SMOKE_ARENA / HNBOOK_SMOKE_QUERIES non posés")
	}
	endpoint := envOr("HNBOOK_ENDPOINT", "http://127.0.0.1:8001/v1/embeddings")
	model := envOr("HNBOOK_MODEL", "qwen3-embedding-0.6b")
	dims, _ := strconv.Atoi(envOr("HNBOOK_DIMS", "512"))

	ar, err := horosvec.OpenArenaReader(arenaPath)
	if err != nil {
		t.Fatalf("OpenArenaReader: %v", err)
	}
	ids, err := readIDs(arenaPath + ".ids")
	if err != nil {
		t.Fatalf("readIDs: %v", err)
	}
	if int64(len(ids)) != ar.Count() {
		t.Fatalf("ids %d != arène count %d", len(ids), ar.Count())
	}
	t.Logf("arène: %d vecteurs, dim %d", ar.Count(), ar.Dim())

	// Build streaming : ArenaPath posé AVANT Build déclenche le chemin vector-less. Le graphe
	// est construit depuis une arène de build (distincte), qui sert ensuite le rerank sans SQL.
	dbPath := t.TempDir() + "/idx.db"
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()

	cfg := horosvec.DefaultConfig()
	cfg.BruteForceThreshold = 0 // force Vamana + rerank arène
	cfg.ArenaPath = dbPath + ".arena"

	idx, err := horosvec.New(db, cfg)
	if err != nil {
		t.Fatalf("horosvec.New: %v", err)
	}
	iter := &arenaIterator{ar: ar, ids: ids, dim: ar.Dim()}
	if err := idx.Build(context.Background(), iter); err != nil {
		t.Fatalf("Build: %v", err)
	}

	// 20 requêtes réelles embeddées en direct.
	queries := loadQueryTexts(t, queriesPath, 20)
	client := newEmbedClient(endpoint, model, dims)
	qvecs, err := client.embedBatch(context.Background(), queries)
	if err != nil {
		t.Fatalf("embed requêtes: %v", err)
	}

	before := idx.RerankSQLLoads()
	nonEmpty := 0
	for i, q := range qvecs {
		res, err := idx.Search(context.Background(), q, 10)
		if err != nil {
			t.Fatalf("Search[%d]: %v", i, err)
		}
		if len(res) > 0 {
			nonEmpty++
		}
	}
	if nonEmpty != len(qvecs) {
		t.Fatalf("Search non vide sur %d/%d requêtes seulement", nonEmpty, len(qvecs))
	}
	if got := idx.RerankSQLLoads() - before; got != 0 {
		t.Fatalf("RerankSQLLoads=%d, veut 0 (le rerank doit lire l'arène, pas SQL)", got)
	}
	t.Logf("smoke C3 OK: %d/%d requêtes non vides, RerankSQLLoads=0", nonEmpty, len(qvecs))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadQueryTexts(t *testing.T, path string, n int) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open queries: %v", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var out []string
	for sc.Scan() && len(out) < n {
		var nl ndjsonLine
		if err := json.Unmarshal(sc.Bytes(), &nl); err != nil {
			t.Fatalf("parse query: %v", err)
		}
		if nl.Text != "" {
			out = append(out, nl.Text)
		}
	}
	if len(out) < n {
		t.Fatalf("seulement %d requêtes, veut %d", len(out), n)
	}
	return out
}
