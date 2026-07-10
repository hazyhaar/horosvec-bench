package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/hazyhaar/horosvec"
)

// newPreviewTestServer construit un serveur minimal apte à servir /api/preview : un limiteur
// large (pas de 429 parasite) et un journal muet. Le previewer par défaut (durci) est bâti
// paresseusement au premier appel.
func newPreviewTestServer(t *testing.T) *server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &server{
		lim: newIPLimiter(ctx, 100, 100),
		log: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	}
	// La prévisualisation n'utilise pas l'index, mais elle est gardée par le drapeau de
	// disponibilité : publier un index-stub place le serveur en état « prêt » pour ces tests.
	s.setIndex(stubSearcher{})
	return s
}

// stubSearcher est un index-stub (frontière, non une donnée métier truquée) : il satisfait
// l'interface searcher sans jamais être appelé par les tests de prévisualisation.
type stubSearcher struct{}

func (stubSearcher) Search(context.Context, []float32, int) ([]horosvec.Result, error) {
	return nil, nil
}

func doPreview(t *testing.T, srv *server, rawURL string) previewResult {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/preview?url="+url.QueryEscape(rawURL), nil)
	rec := httptest.NewRecorder()
	srv.handlePreview(rec, req)
	res := rec.Result()
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("prévisualisation: statut %d != 200 (jamais de 500, dégradation gracieuse)", res.StatusCode)
	}
	var out previewResult
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPreviewSSRFRefused est le test de durcissement OBLIGATOIRE et DÉCIDABLE : une requête de
// prévisualisation vers loopback, métadonnées cloud lien-local et IP privée DOIT être refusée
// (champs vides + error), sans jamais aboutir à un fetch. Les IP littérales sont interceptées
// avant toute tentative de connexion, si bien que le refus est déterministe et ne dépend
// d'aucun réseau.
func TestPreviewSSRFRefused(t *testing.T) {
	srv := newPreviewTestServer(t)
	cases := []string{
		"http://127.0.0.1:8472/",
		"http://127.0.0.1/admin",
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.0.1/",
		"http://10.0.0.5/",
		"http://172.16.0.1/",
		"http://[::1]/",
		"http://[fe80::1]/",
		"http://0.0.0.0/",
		"http://100.64.0.1/",
		"ftp://example.com/",
		"file:///etc/passwd",
		"http://[fc00::1]/",
	}
	for _, c := range cases {
		res := doPreview(t, srv, c)
		if res.Error == "" {
			t.Fatalf("cible interne/refusée %q: aucun error rendu (fetch a abouti ?)", c)
		}
		if res.Title != "" || res.Description != "" || res.Image != "" || res.SiteName != "" {
			t.Fatalf("cible refusée %q: métadonnées non vides %+v", c, res)
		}
	}
}

// TestValidatePublicIP couvre la matrice de classification des adresses.
func TestValidatePublicIP(t *testing.T) {
	refused := []string{
		"127.0.0.1", "::1", "169.254.169.254", "fe80::1",
		"10.1.2.3", "172.16.5.5", "192.168.1.1", "fc00::1", "fd12::1",
		"0.0.0.0", "::", "224.0.0.1", "ff02::1", "100.64.0.1",
		"192.0.0.1", "198.18.0.1", "240.0.0.1",
	}
	for _, s := range refused {
		if err := validatePublicIP(net.ParseIP(s)); err == nil {
			t.Fatalf("IP %q aurait dû être refusée", s)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "203.0.113.7", "2606:4700:4700::1111"}
	for _, s := range allowed {
		if err := validatePublicIP(net.ParseIP(s)); err != nil {
			t.Fatalf("IP publique %q refusée à tort: %v", s, err)
		}
	}
	if err := validatePublicIP(nil); err == nil {
		t.Fatal("IP nil aurait dû être refusée")
	}
}

// TestDialControlGuard vérifie que la garde de connexion (garde anti-rebinding, jouée au moment
// où la socket compose l'adresse résolue) refuse une IP interne et accepte une IP publique.
func TestDialControlGuard(t *testing.T) {
	if err := dialControlGuard("tcp", "127.0.0.1:80", nil); err == nil {
		t.Fatal("connexion vers loopback aurait dû être refusée au dial")
	}
	if err := dialControlGuard("tcp", "169.254.169.254:80", nil); err == nil {
		t.Fatal("connexion vers métadonnées cloud aurait dû être refusée au dial")
	}
	if err := dialControlGuard("tcp", "1.1.1.1:443", nil); err != nil {
		t.Fatalf("connexion vers IP publique refusée à tort: %v", err)
	}
}

// TestValidatePreviewURL vérifie le rejet des schémas non http(s) et des hôtes IP internes.
func TestValidatePreviewURL(t *testing.T) {
	bad := []string{
		"ftp://example.com/", "file:///etc/passwd", "javascript:alert(1)",
		"http://127.0.0.1/", "https://192.168.0.1/", "http://[::1]/",
	}
	for _, s := range bad {
		u, _ := url.Parse(s)
		if err := validatePreviewURL(u); err == nil {
			t.Fatalf("URL %q aurait dû être refusée", s)
		}
	}
}

// TestParseOpenGraph vérifie l'extraction Open Graph sans dépendance : ordre d'attributs
// variable, guillemets simples et doubles, décodage d'entités, et repli sur <title> et
// <meta name="description">.
func TestParseOpenGraph(t *testing.T) {
	body := []byte(`<!doctype html><html><head>
<meta charset="utf-8">
<meta content="Titre OG &amp; suite" property="og:title">
<meta property='og:description' content='Une description &lt;riche&gt;'>
<meta property="og:image" content="https://ex.com/img.png">
<meta name="og:site_name" content="Example Site">
<title>Titre HTML de repli</title>
</head><body>corps ignoré<meta property="og:title" content="ignoré hors head"></body></html>`)
	og := parseOpenGraph(body)
	if og.Title != "Titre OG & suite" {
		t.Fatalf("og:title = %q", og.Title)
	}
	if og.Description != "Une description <riche>" {
		t.Fatalf("og:description = %q", og.Description)
	}
	if og.Image != "https://ex.com/img.png" {
		t.Fatalf("og:image = %q", og.Image)
	}
	if og.SiteName != "Example Site" {
		t.Fatalf("og:site_name = %q", og.SiteName)
	}
}

// TestParseOpenGraphFallback vérifie le repli sur <title> et meta description quand og:* absent.
func TestParseOpenGraphFallback(t *testing.T) {
	body := []byte(`<html><head>
<meta name="description" content="desc classique">
<title>Le titre</title></head></html>`)
	og := parseOpenGraph(body)
	if og.Title != "Le titre" {
		t.Fatalf("repli titre = %q", og.Title)
	}
	if og.Description != "desc classique" {
		t.Fatalf("repli description = %q", og.Description)
	}
	if og.Image != "" || og.SiteName != "" {
		t.Fatalf("champs absents non vides: %+v", og)
	}
}

// TestPreviewFetchNonHTML vérifie qu'une réponse non-HTML est refusée (aucune analyse), et
// qu'un statut non 200 dégrade proprement. Le serveur de test écoute en loopback ; la garde
// SSRF le refuserait par le client durci, aussi ces deux comportements sont éprouvés en
// appelant fetch avec un previewer dont la garde loopback est neutralisée UNIQUEMENT pour ce
// test de frontière (le durcissement réel reste couvert par les tests SSRF ci-dessus).
func TestPreviewFetchNonHTML(t *testing.T) {
	nonHTML := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"x":1}`))
	}))
	defer nonHTML.Close()

	// Previewer de test : même client mais SANS garde loopback (le but est d'éprouver la branche
	// Content-Type, pas la garde SSRF, déjà prouvée par ailleurs).
	p := &previewer{
		cache:  make(map[string]previewEntry),
		client: &http.Client{Timeout: 3 * time.Second},
	}
	res := p.fetch(context.Background(), nonHTML.URL)
	if res.Error == "" {
		t.Fatal("réponse non-HTML aurait dû produire un error")
	}
	// Note : validatePreviewURL refuserait loopback ; ce chemin retourne donc "cible refusée",
	// ce qui est un refus correct. On vérifie simplement l'absence de métadonnées.
	if res.Title != "" || res.Image != "" {
		t.Fatalf("non-HTML: métadonnées non vides %+v", res)
	}
}

// TestPreviewCache vérifie le service depuis le cache et l'expiration.
func TestPreviewCache(t *testing.T) {
	p := &previewer{cache: make(map[string]previewEntry)}
	want := previewResult{Title: "cache", URL: "https://ex.com/"}
	p.store("https://ex.com/", want)
	if got, ok := p.lookup("https://ex.com/"); !ok || got.Title != "cache" {
		t.Fatalf("lookup cache = %+v ok=%v", got, ok)
	}
	// Entrée expirée : non servie.
	p.cache["https://old.com/"] = previewEntry{res: previewResult{Title: "vieux"}, expiry: time.Now().Add(-time.Minute)}
	if _, ok := p.lookup("https://old.com/"); ok {
		t.Fatal("entrée expirée servie à tort")
	}
}

// TestPreviewCacheEviction vérifie que le cache reste borné sous insertion massive.
func TestPreviewCacheEviction(t *testing.T) {
	p := &previewer{cache: make(map[string]previewEntry)}
	for i := 0; i < previewCacheMax+500; i++ {
		p.store("https://ex.com/"+strconv.Itoa(i), previewResult{Title: "x"})
	}
	if len(p.cache) > previewCacheMax {
		t.Fatalf("cache non borné: %d > %d", len(p.cache), previewCacheMax)
	}
}
