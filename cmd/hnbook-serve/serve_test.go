package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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

func newTestServer(t *testing.T, idx *horosvec.Index, embedURL string, rate, burst float64) *server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &server{
		idx:        idx,
		embed:      &embedClient{url: embedURL, hc: &http.Client{Timeout: 2 * time.Second}},
		lim:        newIPLimiter(ctx, rate, burst),
		reqTimeout: 5 * time.Second,
		topK:       10,
		html:       []byte("<html></html>"),
		log:        slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
}

func doSearch(t *testing.T, srv *server, q string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/search?q="+url.QueryEscape(q), nil)
	rec := httptest.NewRecorder()
	srv.handleSearch(rec, req)
	return rec.Result()
}

// TestSearchOK vérifie qu'une recherche nominale rend un top-K non vide, une latence
// renseignée, et l'ext_id du corpus (le plus proche du vecteur d'un item est l'item lui-même).
func TestSearchOK(t *testing.T) {
	idx, q0 := buildTestIndex(t, 300)
	side := fakeSidecar(t, q0)
	srv := newTestServer(t, idx, side.URL, 100, 100)

	resp := doSearch(t, srv, "sujet de test")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("statut %d != 200", resp.StatusCode)
	}
	var out searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 10 {
		t.Fatalf("résultats %d != 10", len(out.Results))
	}
	if out.LatencyMS <= 0 {
		t.Fatalf("latence non renseignée: %.3f", out.LatencyMS)
	}
	if out.EmbedMS < 0 {
		t.Fatalf("embed_ms négatif: %.3f", out.EmbedMS)
	}
	if out.Results[0].ID != "0" {
		t.Fatalf("plus proche de q0 = %q, attendu \"0\"", out.Results[0].ID)
	}
}

// TestRateLimit vérifie que le limiteur déclenche un 429 une fois le burst épuisé.
func TestRateLimit(t *testing.T) {
	idx, q0 := buildTestIndex(t, 100)
	side := fakeSidecar(t, q0)
	srv := newTestServer(t, idx, side.URL, 0.001, 5) // burst 5, régénération négligeable

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

	srv := newTestServer(t, idx, deadURL, 100, 100)
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
	srv := newTestServer(t, idx, side.URL, 100, 100)

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

// TestLoadTitles vérifie le chargement, la troncature et le repli map nil.
func TestLoadTitles(t *testing.T) {
	if m, err := loadTitles(""); err != nil || m != nil {
		t.Fatalf("chemin vide: map=%v err=%v (attendu nil,nil)", m, err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "titles.tsv")
	long := ""
	for i := 0; i < maxTitleLen+50; i++ {
		long += "x"
	}
	content := "123\tUn titre HN\n456\t" + long + "\n\n789\t  \n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := loadTitles(p)
	if err != nil {
		t.Fatal(err)
	}
	if m["123"] != "Un titre HN" {
		t.Fatalf("titre 123 = %q", m["123"])
	}
	if r := []rune(m["456"]); len(r) != maxTitleLen+1 || r[len(r)-1] != '…' {
		t.Fatalf("titre 456 non tronqué à l'ellipse: %d runes", len(r))
	}
	if _, ok := m["789"]; ok {
		t.Fatal("ligne à titre vide indûment retenue")
	}
}

// TestLoadTitlesMalformed vérifie l'échec fail-loud sur ligne sans tabulation.
func TestLoadTitlesMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "bad.tsv")
	if err := os.WriteFile(p, []byte("pas_de_tabulation_ici\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTitles(p); err == nil {
		t.Fatal("attendu une erreur sur ligne sans tabulation")
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

// TestTitleSnippetInResults vérifie que le titre chargé est restitué dans la réponse.
func TestTitleSnippetInResults(t *testing.T) {
	idx, q0 := buildTestIndex(t, 120)
	side := fakeSidecar(t, q0)
	srv := newTestServer(t, idx, side.URL, 100, 100)
	srv.titles = map[string]string{"0": "Titre de l'item zéro"}

	resp := doSearch(t, srv, "requête")
	defer resp.Body.Close()
	var out searchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Results[0].ID != "0" || out.Results[0].TitleSnippet != "Titre de l'item zéro" {
		t.Fatalf("snippet manquant: %+v", out.Results[0])
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

// TestHealthz vérifie la sonde de vivacité.
func TestHealthz(t *testing.T) {
	srv := &server{log: slog.New(slog.NewJSONHandler(io.Discard, nil))}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.handleHealthz(rec, req)
	if rec.Result().StatusCode != http.StatusOK {
		t.Fatalf("healthz statut %d != 200", rec.Result().StatusCode)
	}
}
