package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hazyhaar/horosvec"
	_ "modernc.org/sqlite"
)

// sliceIter est un VectorIterator sur des vecteurs en tranche (ext_id = rang décimal ASCII,
// comme le grave le chemin arène de horosvec) — même stub que le harnais de hnbook-validate.
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

// buildTestIndex construit un index horosvec réel (chemin arène) de dimension embedDim sur n
// vecteurs unitaires, et retourne l'index ouvert prêt à servir ainsi que le premier vecteur.
// hnSliceIter assigne des ext_id HN explicites (chaîne décimale) aux vecteurs de test.
type hnSliceIter struct {
	ids  []string
	vecs [][]float32
	pos  int
}

func (s *hnSliceIter) Next() (id []byte, vec []float32, ok bool) {
	if s.pos >= len(s.vecs) {
		return nil, nil, false
	}
	i := s.pos
	s.pos++
	return []byte(s.ids[i]), s.vecs[i], true
}

func (s *hnSliceIter) Reset() error { s.pos = 0; return nil }

func buildStoreMatchedIndex(t *testing.T) (*horosvec.Index, []float32) {
	t.Helper()
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.db")
	arenaPath := filepath.Join(dir, "corpus.arena")
	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := horosvec.DefaultConfig()
	cfg.ArenaPath = arenaPath
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"1000", "1001", "1002", "2000", "3000", "3001"}
	rng := rand.New(rand.NewPCG(20260713, 3))
	vecs := make([][]float32, len(ids))
	for i := range vecs {
		v := make([]float32, embedDim)
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
	if err := idx.Build(context.Background(), &hnSliceIter{ids: ids, vecs: vecs}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = idx.Close()
		_ = db.Close()
	})
	return idx, vecs[0]
}

func buildTestIndex(t *testing.T, n int) (*horosvec.Index, []float32) {
	t.Helper()
	dir := t.TempDir()
	indexPath := filepath.Join(dir, "index.db")
	arenaPath := filepath.Join(dir, "corpus.arena")

	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := horosvec.DefaultConfig()
	cfg.ArenaPath = arenaPath
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(20260709, 11))
	vecs := make([][]float32, n)
	for i := range vecs {
		v := make([]float32, embedDim)
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
	t.Cleanup(func() {
		_ = idx.Close()
		_ = db.Close()
	})
	return idx, vecs[0]
}

// fakeSidecar est un STUB de frontière process (pas une donnée métier truquée) : un serveur
// HTTP de test rendant un vecteur normalisé de dimension embedDim, exactement comme le contrat
// du sidecar réel. Il retourne le vecteur fourni (typiquement un vecteur réel du corpus).
func fakeSidecar(t *testing.T, vec []float32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string][]float32{"vector": vec})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestServer(t *testing.T, idx searcher, store *titleStore, embedURL string, rate, burst float64) *server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &server{
		embed:      &embedClient{url: embedURL, hc: &http.Client{Timeout: 2 * time.Second}},
		store:      store,
		lim:        newIPLimiter(ctx, rate, burst),
		reqTimeout: 5 * time.Second,
		topK:       10,
		kRaw:       300,
		tau:        0.1,
		html:       []byte("<html></html>"),
		warming:    []byte("<html>warming</html>"),
		log:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	s.setIndex(idx)
	return s
}

// fakeSearcher renvoie des hits vectoriels connus (tests de regroupement/score).
type fakeSearcher struct {
	hits []horosvec.Result
}

func (f *fakeSearcher) Search(_ context.Context, _ []float32, topK int) ([]horosvec.Result, error) {
	n := topK
	if n > len(f.hits) {
		n = len(f.hits)
	}
	return f.hits[:n], nil
}

// errSearcher simule un sous-index qui échoue à la recherche.
type errSearcher struct {
	err error
}

func (e *errSearcher) Search(context.Context, []float32, int) ([]horosvec.Result, error) {
	if e.err == nil {
		return nil, errors.New("search failed")
	}
	return nil, e.err
}

func buildTestStore(t *testing.T) *titleStore {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	longText := strings.Repeat("α", 150) + " &lt;end&gt;"
	ddl := `
CREATE TABLE item(
  id INTEGER PRIMARY KEY,
  ts INTEGER,
  type TEXT,
  title TEXT,
  parent INTEGER,
  root_id INTEGER,
  depth INTEGER,
  orphan INTEGER DEFAULT 0,
  text TEXT
);`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	rows := []struct {
		id, ts, parent, root, depth, orphan int64
		typ, title, text                    string
	}{
		{1000, 1609459200, 0, 1000, 0, 0, "story", "Story Alpha", ""},
		{1001, 1609459300, 1000, 1000, 1, 0, "comment", "Comment one", longText},
		{1002, 1609459400, 1001, 1000, 2, 0, "comment", "Comment two", "Short reply &amp; done"},
		{2000, 1609545600, 0, 2000, 0, 0, "story", "Orphan story", ""},
		{3000, 1609632000, 0, 3000, 0, 0, "story", "Story Beta", ""},
		{3001, 1609632100, 3000, 3000, 1, 0, "comment", "Beta comment only", "Beta body text"},
	}
	for _, r := range rows {
		_, err := db.Exec(`INSERT INTO item(id,ts,type,title,parent,root_id,depth,orphan,text) VALUES(?,?,?,?,?,?,?,?,?)`,
			r.id, r.ts, r.typ, r.title, r.parent, r.root, r.depth, r.orphan, r.text)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := openTitleStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func doSearch(t *testing.T, srv *server, q string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+url.QueryEscape(q), nil)
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, req)
	return rec.Result()
}

func decodeSearch(t *testing.T, resp *http.Response) searchResponse {
	t.Helper()
	var out searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestFederatedMerge vérifie la fusion k_raw par index : tri d², dédup ext_id, hits monolithe+shard.
func TestFederatedMerge(t *testing.T) {
	mono := &fakeSearcher{hits: []horosvec.Result{
		{ID: []byte("1000"), Score: 0.5},
		{ID: []byte("1001"), Score: 0.3},
	}}
	shard := &fakeSearcher{hits: []horosvec.Result{
		{ID: []byte("9000"), Score: 0.2},
		{ID: []byte("1001"), Score: 0.9},
	}}
	fs := newFederatedSearcher(slog.New(slog.NewJSONHandler(io.Discard, nil)), []labeledSearcher{
		{label: "monolith", s: mono},
		{label: "shard", s: shard},
	})
	got, err := fs.Search(context.Background(), nil, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("hits %d != 3", len(got))
	}
	want := []struct {
		id string
		d2 float64
	}{
		{"9000", 0.2},
		{"1001", 0.3},
		{"1000", 0.5},
	}
	for i, w := range want {
		if string(got[i].ID) != w.id || got[i].Score != w.d2 {
			t.Fatalf("rang %d: %+v, attendu id=%s d2=%v", i, got[i], w.id, w.d2)
		}
	}
	indices := fs.LastIndices()
	if len(indices) != 2 || indices[0] != "monolith" || indices[1] != "shard" {
		t.Fatalf("indices = %v", indices)
	}
}

// TestFederatedDegradation vérifie qu'un sous-searcher en erreur est sauté et signalé.
func TestFederatedDegradation(t *testing.T) {
	fs := newFederatedSearcher(slog.New(slog.NewJSONHandler(io.Discard, nil)), []labeledSearcher{
		{label: "monolith", s: &fakeSearcher{hits: []horosvec.Result{{ID: []byte("1000"), Score: 0.1}}}},
		{label: "broken", s: &errSearcher{err: errors.New("index unavailable")}},
	})
	got, err := fs.Search(context.Background(), nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0].ID) != "1000" {
		t.Fatalf("résultat inattendu: %+v", got)
	}
	if skipped := fs.LastSkipped(); len(skipped) != 1 || skipped[0] != "broken" {
		t.Fatalf("skipped = %v, attendu [broken]", skipped)
	}
}

// TestFederatedDedup vérifie qu'un même ext_id dans deux index ne garde que le plus petit d².
func TestFederatedDedup(t *testing.T) {
	fs := newFederatedSearcher(slog.New(slog.NewJSONHandler(io.Discard, nil)), []labeledSearcher{
		{label: "a", s: &fakeSearcher{hits: []horosvec.Result{{ID: []byte("42"), Score: 0.8}}}},
		{label: "b", s: &fakeSearcher{hits: []horosvec.Result{{ID: []byte("42"), Score: 0.2}}}},
	})
	got, err := fs.Search(context.Background(), nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Score != 0.2 {
		t.Fatalf("dédup: %+v", got)
	}
}

// TestFederatedSearchAPI expose indices/skipped dans la réponse JSON.
func TestFederatedSearchAPI(t *testing.T) {
	store := buildTestStore(t)
	fs := newFederatedSearcher(slog.New(slog.NewJSONHandler(io.Discard, nil)), []labeledSearcher{
		{label: "monolith", s: &fakeSearcher{hits: []horosvec.Result{{ID: []byte("1001"), Score: 0.1}}}},
		{label: "shard", s: &fakeSearcher{hits: []horosvec.Result{{ID: []byte("3001"), Score: 0.2}}}},
	})
	side := fakeSidecar(t, make([]float32, embedDim))
	srv := newTestServer(t, fs, store, side.URL, 100, 100)

	resp := doSearch(t, srv, "requête")
	defer resp.Body.Close()
	out := decodeSearch(t, resp)
	if len(out.Indices) != 2 || out.Indices[0] != "monolith" || out.Indices[1] != "shard" {
		t.Fatalf("indices = %v", out.Indices)
	}
	if len(out.Threads) < 1 {
		t.Fatal("aucun fil rendu")
	}
}

// TestParseTopK vérifie la résolution et l'écrêtage du paramètre k au niveau unitaire.
func TestParseTopK(t *testing.T) {
	cases := []struct {
		raw      string
		fallback int
		want     int
	}{
		{"", 10, 10},         // absent -> fallback
		{"  ", 10, 10},       // vide -> fallback
		{"abc", 10, 10},      // invalide -> fallback
		{"0", 10, 10},        // <= 0 -> fallback
		{"-5", 10, 10},       // négatif -> fallback
		{"60", 10, 60},       // valide dans la borne
		{"100", 10, 100},     // à la borne
		{"500", 10, 100},     // au-delà -> écrêté à maxTopK
		{"1000000", 10, 100}, // très grand -> écrêté
	}
	for _, c := range cases {
		if got := parseTopK(c.raw, c.fallback); got != c.want {
			t.Errorf("parseTopK(%q,%d)=%d, attendu %d", c.raw, c.fallback, got, c.want)
		}
	}
}

// TestSearchOK vérifie qu'une recherche nominale rend des fils, la fraîcheur, et une latence.
func TestSearchOK(t *testing.T) {
	store := buildTestStore(t)
	fs := &fakeSearcher{hits: []horosvec.Result{
		{ID: []byte("1001"), Score: 0.2},
		{ID: []byte("3001"), Score: 0.5},
	}}
	side := fakeSidecar(t, make([]float32, embedDim))
	srv := newTestServer(t, fs, store, side.URL, 100, 100)

	resp := doSearch(t, srv, "sujet de test")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statut %d != 200", resp.StatusCode)
	}
	out := decodeSearch(t, resp)
	if len(out.Threads) != 2 {
		t.Fatalf("threads %d != 2", len(out.Threads))
	}
	if out.Freshness == "" {
		t.Fatal("freshness manquante")
	}
	if out.LatencyMS <= 0 {
		t.Fatalf("latence non renseignée: %.3f", out.LatencyMS)
	}
	if out.EmbedMS < 0 {
		t.Fatalf("embed_ms négatif: %.3f", out.EmbedMS)
	}
}

// TestRateLimit vérifie que le limiteur déclenche un 429 une fois le burst épuisé.
func TestRateLimit(t *testing.T) {
	idx, q0 := buildTestIndex(t, 100)
	side := fakeSidecar(t, q0)
	srv := newTestServer(t, idx, buildTestStore(t), side.URL, 0.001, 5) // burst 5, régénération négligeable

	got429 := false
	for i := 0; i < 12; i++ {
		resp := doSearch(t, srv, "requête")
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	if !got429 {
		t.Fatal("aucun 429 après épuisement du burst")
	}
}

// TestSidecarDown vérifie qu'un sidecar injoignable produit un 503 propre (jamais un résultat
// vide silencieux).
func TestSidecarDown(t *testing.T) {
	idx, q0 := buildTestIndex(t, 50)
	side := fakeSidecar(t, q0)
	deadURL := side.URL
	side.Close() // ferme le sidecar : connexion refusée

	srv := newTestServer(t, idx, buildTestStore(t), deadURL, 100, 100)
	resp := doSearch(t, srv, "requête")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("statut %d != 503", resp.StatusCode)
	}
}

// TestQueryBounds vérifie les bornes d'entrée : q absent -> 400, q trop long -> 400.
func TestQueryBounds(t *testing.T) {
	idx, q0 := buildTestIndex(t, 50)
	side := fakeSidecar(t, q0)
	srv := newTestServer(t, idx, buildTestStore(t), side.URL, 100, 100)

	resp := doSearch(t, srv, "")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("q vide: statut %d != 400", resp.StatusCode)
	}

	long := make([]byte, maxQueryBytes+1)
	for i := range long {
		long[i] = 'a'
	}
	resp = doSearch(t, srv, string(long))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("q trop long: statut %d != 400", resp.StatusCode)
	}
}

// TestTruncateTitle vérifie la troncature à l'ellipse.
func TestTruncateTitle(t *testing.T) {
	long := strings.Repeat("x", maxTitleLen+50)
	got := truncateTitle(long)
	r := []rune(got)
	if len(r) != maxTitleLen+1 || r[len(r)-1] != '…' {
		t.Fatalf("troncature incorrecte: %d runes", len(r))
	}
	if truncateTitle("court") != "court" {
		t.Fatal("titre court modifié")
	}
}

// TestIndexPage vérifie que la page embarquée est servie en text/html.
func TestIndexPage(t *testing.T) {
	srv := &server{html: []byte("<html>ok</html>"), log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.handleIndex(rec, req)
	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("statut %d != 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type %q", ct)
	}
}

// TestThreadGroupingScore vérifie le regroupement par root_id et le score log-somme-exp.
func TestThreadGroupingScore(t *testing.T) {
	store := buildTestStore(t)
	fs := &fakeSearcher{hits: []horosvec.Result{
		{ID: []byte("1001"), Score: 0.1},
		{ID: []byte("1002"), Score: 0.2},
		{ID: []byte("2000"), Score: 0.1},
		{ID: []byte("3001"), Score: 0.4},
	}}
	side := fakeSidecar(t, make([]float32, embedDim))
	srv := newTestServer(t, fs, store, side.URL, 100, 100)

	resp := doSearch(t, srv, "requête")
	defer resp.Body.Close()
	out := decodeSearch(t, resp)
	if len(out.Threads) != 3 {
		t.Fatalf("threads %d != 3", len(out.Threads))
	}
	if out.Threads[0].Root.ID != "1000" {
		t.Fatalf("premier fil = %s, attendu 1000 (2 hits forts)", out.Threads[0].Root.ID)
	}
	if len(out.Threads[0].Hits) != 2 {
		t.Fatalf("hits fil 1000 = %d, attendu 2", len(out.Threads[0].Hits))
	}
	single := 0.0
	double := 0.0
	for _, th := range out.Threads {
		switch th.Root.ID {
		case "1000":
			double = th.Score
		case "2000":
			single = th.Score
		}
	}
	if double <= single {
		t.Fatalf("fil 2 hits (%.4f) doit battre fil 1 hit même d² (%.4f)", double, single)
	}
}

// TestThreadRootTitleWithoutStoryHit vérifie le titre racine joint sans hit story.
func TestThreadRootTitleWithoutStoryHit(t *testing.T) {
	store := buildTestStore(t)
	fs := &fakeSearcher{hits: []horosvec.Result{{ID: []byte("3001"), Score: 0.3}}}
	side := fakeSidecar(t, make([]float32, embedDim))
	srv := newTestServer(t, fs, store, side.URL, 100, 100)

	resp := doSearch(t, srv, "requête")
	defer resp.Body.Close()
	out := decodeSearch(t, resp)
	if len(out.Threads) != 1 {
		t.Fatalf("threads %d != 1", len(out.Threads))
	}
	if out.Threads[0].Root.Title != "Story Beta" {
		t.Fatalf("titre racine = %q, attendu Story Beta", out.Threads[0].Root.Title)
	}
	if out.Threads[0].Root.URL != "https://news.ycombinator.com/item?id=3000" {
		t.Fatalf("url racine incorrecte: %s", out.Threads[0].Root.URL)
	}
}

// TestAPISearchSmokeJSON expose la réponse /api/search (preuve locale type curl).
func TestAPISearchSmokeJSON(t *testing.T) {
	store := buildTestStore(t)
	idx, q0 := buildStoreMatchedIndex(t)
	side := fakeSidecar(t, q0)
	srv := newTestServer(t, idx, store, side.URL, 100, 100)

	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/search?q=machine+learning")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statut %d: %s", resp.StatusCode, body)
	}
	var pretty map[string]any
	if err := json.Unmarshal(body, &pretty); err != nil {
		t.Fatal(err)
	}
	enc, _ := json.MarshalIndent(pretty, "", "  ")
	t.Logf("SMOKE /api/search JSON:\n%s", enc)
}

// TestThreadSingleBatchQueries vérifie exactement une requête IN par batch (pas N lookups).
func TestThreadSingleBatchQueries(t *testing.T) {
	store := buildTestStore(t)
	fs := &fakeSearcher{hits: []horosvec.Result{
		{ID: []byte("1001"), Score: 0.1},
		{ID: []byte("1002"), Score: 0.2},
		{ID: []byte("3001"), Score: 0.3},
	}}
	side := fakeSidecar(t, make([]float32, embedDim))
	srv := newTestServer(t, fs, store, side.URL, 100, 100)
	store.batchHits.Store(0)
	store.batchRoot.Store(0)

	resp := doSearch(t, srv, "requête")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statut %d != 200", resp.StatusCode)
	}
	if got := store.batchHits.Load(); got != 1 {
		t.Fatalf("requêtes batch hits = %d, attendu 1", got)
	}
	if got := store.batchRoot.Load(); got != 1 {
		t.Fatalf("requêtes batch racines = %d, attendu 1", got)
	}
}

// TestClientIP vérifie que X-Forwarded-For n'est retenu qu'en mode proxy de confiance, et que
// c'est alors le maillon le plus à droite (IP réelle apposée par nginx), jamais la valeur
// falsifiable de gauche.
func TestClientIP(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/search?q=x", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.7")

	if ip := clientIP(r, false); ip != "10.0.0.9" {
		t.Fatalf("sans confiance proxy: %q, attendu RemoteAddr 10.0.0.9", ip)
	}
	if ip := clientIP(r, true); ip != "203.0.113.7" {
		t.Fatalf("avec confiance proxy: %q, attendu le maillon droit 203.0.113.7", ip)
	}
	// Valeur de gauche falsifiée par l'attaquant : jamais retenue.
	if ip := clientIP(r, true); ip == "1.2.3.4" {
		t.Fatal("le maillon gauche falsifiable a été retenu")
	}
}

// TestHealthz vérifie que /healthz est une sonde de disponibilité : 200 une fois l'index prêt.
func TestHealthz(t *testing.T) {
	idx, q0 := buildTestIndex(t, 50)
	side := fakeSidecar(t, q0)
	srv := newTestServer(t, idx, buildTestStore(t), side.URL, 100, 100)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.handleHealthz(rec, req)
	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("healthz prêt: statut %d != 200", rec.Result().StatusCode)
	}
}

// TestWarmingState est le test DÉCIDABLE de l'état préchauffage : un serveur dont l'index n'est
// pas encore publié rend 200 + page de préchauffage sur GET /, 503 + {"status":"warming"} +
// Retry-After sur /api/search et /api/preview, et 503 sur /healthz ; après bascule à prêt,
// /api/search répond normalement et /healthz vaut 200.
func TestWarmingState(t *testing.T) {
	side := fakeSidecar(t, make([]float32, embedDim))
	srv := &server{
		embed:      &embedClient{url: side.URL, hc: &http.Client{Timeout: 2 * time.Second}},
		lim:        newIPLimiter(context.Background(), 100, 100),
		reqTimeout: 5 * time.Second,
		topK:       10,
		html:       []byte("<html>prêt</html>"),
		warming:    []byte("<html>préchauffage</html>"),
		log:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}

	// État préchauffage : index non publié.
	rec := httptest.NewRecorder()
	srv.handleIndex(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / en préchauffage: statut %d != 200", res.StatusCode)
	}
	if body, _ := io.ReadAll(res.Body); string(body) != "<html>préchauffage</html>" {
		t.Fatalf("GET / en préchauffage ne rend pas la page warming: %q", body)
	}

	rec = httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=x", nil))
	res = rec.Result()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/api/search en préchauffage: statut %d != 503", res.StatusCode)
	}
	if res.Header.Get("Retry-After") != "5" {
		t.Fatalf("/api/search en préchauffage: Retry-After=%q != 5", res.Header.Get("Retry-After"))
	}
	var warmBody map[string]string
	if err := json.NewDecoder(res.Body).Decode(&warmBody); err != nil {
		t.Fatal(err)
	}
	if warmBody["status"] != "warming" {
		t.Fatalf("/api/search en préchauffage: status=%q != warming", warmBody["status"])
	}

	rec = httptest.NewRecorder()
	srv.handlePreview(rec, httptest.NewRequest(http.MethodGet, "/api/preview?url=x", nil))
	if rec.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/api/preview en préchauffage: statut %d != 503", rec.Result().StatusCode)
	}

	rec = httptest.NewRecorder()
	srv.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/healthz en préchauffage: statut %d != 503", rec.Result().StatusCode)
	}

	// Bascule à prêt.
	store := buildTestStore(t)
	fs := &fakeSearcher{hits: []horosvec.Result{{ID: []byte("1000"), Score: 0.1}}}
	srv.store = store
	srv.setIndex(fs)

	rec = httptest.NewRecorder()
	srv.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("/healthz une fois prêt: statut %d != 200", rec.Result().StatusCode)
	}

	rec = httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=x", nil))
	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("/api/search une fois prêt: statut %d != 200", rec.Result().StatusCode)
	}
}

// TestItemByID vérifie que itemByID restitue le texte stocké.
func TestItemByID(t *testing.T) {
	store := buildTestStore(t)
	ctx := context.Background()

	it, ok, err := store.itemByID(ctx, 1002)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("item 1002 not found")
	}
	if it.Text != "Short reply &amp; done" {
		t.Fatalf("text = %q", it.Text)
	}
	if it.Title != "Comment two" {
		t.Fatalf("title = %q", it.Title)
	}
}

// TestAPIItemRoute vérifie GET /api/item?id=.
func TestAPIItemRoute(t *testing.T) {
	store := buildTestStore(t)
	fs := &fakeSearcher{}
	side := fakeSidecar(t, make([]float32, embedDim))
	srv := newTestServer(t, fs, store, side.URL, 100, 100)

	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/api/item?id=1002")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statut %d != 200", resp.StatusCode)
	}
	var out itemResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "1002" || out.Text != "Short reply &amp; done" {
		t.Fatalf("réponse inattendue: %+v", out)
	}
	if out.URL != "https://news.ycombinator.com/item?id=1002" {
		t.Fatalf("url = %q", out.URL)
	}
}

// TestTextSnippetInSearch vérifie text_snippet décodé et tronqué sur les hits commentaires.
func TestTextSnippetInSearch(t *testing.T) {
	store := buildTestStore(t)
	fs := &fakeSearcher{hits: []horosvec.Result{{ID: []byte("1001"), Score: 0.1}}}
	side := fakeSidecar(t, make([]float32, embedDim))
	srv := newTestServer(t, fs, store, side.URL, 100, 100)

	resp := doSearch(t, srv, "requête")
	defer resp.Body.Close()
	out := decodeSearch(t, resp)
	if len(out.Threads) != 1 {
		t.Fatalf("threads %d != 1", len(out.Threads))
	}
	if len(out.Threads[0].Hits) != 1 {
		t.Fatalf("hits %d != 1", len(out.Threads[0].Hits))
	}
	snip := out.Threads[0].Hits[0].TextSnippet
	if snip == "" {
		t.Fatal("text_snippet manquant")
	}
	r := []rune(snip)
	if len(r) != maxTextSnippetLen+1 || r[len(r)-1] != '…' {
		t.Fatalf("troncature incorrecte: %d runes", len(r))
	}
	if strings.Contains(snip, "&lt;") {
		t.Fatalf("entités non décodées: %q", snip)
	}
}

// TestTruncateTextSnippet vérifie le décodage HN et la borne à 140 runes.
func TestTruncateTextSnippet(t *testing.T) {
	raw := strings.Repeat("z", 200) + " &amp; done"
	got := truncateTextSnippet(raw)
	gr := []rune(got)
	if len(gr) != maxTextSnippetLen+1 {
		t.Fatalf("longueur %d != %d+1", len(gr), maxTextSnippetLen)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("pas d'ellipse: %q", got)
	}
	if strings.Contains(got, "&amp;") {
		t.Fatal("entité non décodée")
	}
}

// TestWarmingLoadError vérifie qu'un échec de chargement bascule les routes en 503 avec message
// d'erreur (jamais un préchauffage éternel qui prétendrait la disponibilité imminente).
func TestWarmingLoadError(t *testing.T) {
	srv := &server{
		lim:     newIPLimiter(context.Background(), 100, 100),
		html:    []byte("<html>prêt</html>"),
		warming: []byte("<html>préchauffage</html>"),
		log:     slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	msg := "chargement de l'index impossible"
	srv.loadErr.Store(&msg)

	rec := httptest.NewRecorder()
	srv.handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Result().StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/healthz en erreur: statut %d != 503", rec.Result().StatusCode)
	}

	rec = httptest.NewRecorder()
	srv.handleSearch(rec, httptest.NewRequest(http.MethodGet, "/api/search?q=x", nil))
	res := rec.Result()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/api/search en erreur: statut %d != 503", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "error" {
		t.Fatalf("/api/search en erreur: status=%q != error", body["status"])
	}
}
