package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hazyhaar/horosvec"
)

// fakeEmbedServer rend un embedding déterministe : vec[0] = entier extrait du texte
// "item <n>", le reste constant. Cela permet de vérifier au sol l'appariement rang→id→vecteur
// (aucune dérive, aucun doublon, aucun trou). Un seuil optionnel déclenche cancel après un
// nombre de requêtes servies, pour simuler une interruption réelle en cours de run.
func fakeEmbedServer(t *testing.T, dim int, cancelAfter int32, cancel context.CancelFunc) *httptest.Server {
	t.Helper()
	var served int32
	h := func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		items := make([]embedResponseItem, len(req.Input))
		for i, text := range req.Input {
			n, err := strconv.Atoi(strings.TrimPrefix(text, "item "))
			if err != nil {
				http.Error(w, "bad text "+text, http.StatusBadRequest)
				return
			}
			vec := make([]float32, dim)
			vec[0] = float32(n)
			for j := 1; j < dim; j++ {
				vec[j] = 0.25
			}
			items[i] = embedResponseItem{Index: i, Embedding: vec}
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Data: items})
		if cancelAfter > 0 && atomic.AddInt32(&served, 1) >= cancelAfter {
			cancel()
		}
	}
	return httptest.NewServer(http.HandlerFunc(h))
}

// makeNDJSON produit N lignes {"id":1000+r,"text":"item <1000+r>"}.
func makeNDJSON(n int) string {
	var sb strings.Builder
	for r := 0; r < n; r++ {
		id := 1000 + r
		fmt.Fprintf(&sb, "{\"id\":%d,\"text\":\"item %d\"}\n", id, id)
	}
	return sb.String()
}

func baseCfg(dir, endpoint string, dim int) pipelineConfig {
	arena := filepath.Join(dir, "a.arena")
	return pipelineConfig{
		arenaPath:        arena,
		idsPath:          arena + ".ids",
		manifestPath:     arena + ".manifest.json",
		endpoint:         endpoint,
		model:            "test",
		dims:             dim,
		batchSize:        100,
		concurrency:      1,
		checkpointEvery:  1,
		progressInterval: time.Hour,
	}
}

// verifyComplete vérifie qu'arène + ids sont finalisés, complets (N), alignés (vec[0]==id) et
// dans l'ordre du flux (id==1000+rang), sans doublon ni trou.
func verifyComplete(t *testing.T, cfg pipelineConfig, n, dim int) {
	t.Helper()
	if _, err := os.Stat(cfg.arenaPath + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("le .tmp devrait avoir disparu après finalize: %v", err)
	}
	ids, err := readIDs(cfg.idsPath)
	if err != nil {
		t.Fatalf("readIDs: %v", err)
	}
	if len(ids) != n {
		t.Fatalf("ids count %d != %d", len(ids), n)
	}
	ar, err := horosvec.OpenArenaReader(cfg.arenaPath)
	if err != nil {
		t.Fatalf("OpenArenaReader: %v", err)
	}
	if ar.Count() != int64(n) {
		t.Fatalf("arène count %d != %d", ar.Count(), n)
	}
	if ar.Dim() != dim {
		t.Fatalf("arène dim %d != %d", ar.Dim(), dim)
	}
	dst := make([]float32, dim)
	for r := 0; r < n; r++ {
		wantID := uint64(1000 + r)
		if ids[r] != wantID {
			t.Fatalf("ids[%d]=%d, veut %d (ordre/dérive)", r, ids[r], wantID)
		}
		if !ar.VecInto(int64(r), dst) {
			t.Fatalf("VecInto(%d) hors bornes", r)
		}
		if dst[0] != float32(wantID) {
			t.Fatalf("arène[%d][0]=%v, veut %v (appariement arène↔ids rompu)", r, dst[0], float32(wantID))
		}
	}
	m, err := readManifest(cfg.manifestPath)
	if err != nil || m == nil {
		t.Fatalf("readManifest: %v (m=%v)", err, m)
	}
	if m.Status != statusDone || m.Count != int64(n) {
		t.Fatalf("manifest status=%q count=%d, veut done/%d", m.Status, m.Count, n)
	}
}

func TestPipelineFullRun(t *testing.T) {
	dim := 8
	n := 350
	dir := t.TempDir()
	srv := fakeEmbedServer(t, dim, 0, nil)
	defer srv.Close()
	cfg := baseCfg(dir, srv.URL, dim)

	if err := runPipeline(context.Background(), cfg, strings.NewReader(makeNDJSON(n))); err != nil {
		t.Fatalf("runPipeline: %v", err)
	}
	verifyComplete(t, cfg, n, dim)
}

// TestPipelineCheckpointResume simule une interruption réelle en cours de run (cancel du
// contexte après quelques lots servis), vérifie que l'état partiel est un checkpoint valide
// (multiple de la taille de lot, < N, .tmp présent, pas d'arène finale), puis relance : la
// reprise complète l'arène sans doublon ni trou.
func TestPipelineCheckpointResume(t *testing.T) {
	dim := 8
	n := 500
	dir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	srv := fakeEmbedServer(t, dim, 2, cancel) // cancel après 2 lots servis
	cfg := baseCfg(dir, srv.URL, dim)

	err := runPipeline(ctx, cfg, strings.NewReader(makeNDJSON(n)))
	if err == nil {
		t.Fatal("le premier run interrompu devrait retourner une erreur (contexte annulé)")
	}
	srv.Close()

	// État partiel : .tmp présent, pas d'arène finale, manifest checkpoint valide.
	if _, e := os.Stat(cfg.arenaPath); !os.IsNotExist(e) {
		t.Fatalf("aucune arène finale ne devrait exister après interruption: %v", e)
	}
	if _, e := os.Stat(cfg.arenaPath + ".tmp"); e != nil {
		t.Fatalf("le .tmp de reprise devrait exister: %v", e)
	}
	m, e := readManifest(cfg.manifestPath)
	if e != nil || m == nil {
		t.Fatalf("manifest checkpoint absent: %v", e)
	}
	if m.Status != statusInProgress {
		t.Fatalf("manifest devrait être in_progress, a %q", m.Status)
	}
	if m.Count == 0 || m.Count >= int64(n) || m.Count%int64(cfg.batchSize) != 0 {
		t.Fatalf("checkpoint count %d invalide (attendu multiple de %d, dans ]0,%d[)", m.Count, cfg.batchSize, n)
	}
	t.Logf("interruption au checkpoint count=%d", m.Count)

	// Reprise : nouveau serveur sans cancel, contexte frais.
	srv2 := fakeEmbedServer(t, dim, 0, nil)
	defer srv2.Close()
	cfg.endpoint = srv2.URL
	if err := runPipeline(context.Background(), cfg, strings.NewReader(makeNDJSON(n))); err != nil {
		t.Fatalf("reprise: %v", err)
	}
	verifyComplete(t, cfg, n, dim)
}

// TestPipelineResumeIdempotentDone : un manifest done fait retourner sans réembedder.
func TestPipelineResumeAlreadyDone(t *testing.T) {
	dim := 8
	n := 200
	dir := t.TempDir()
	srv := fakeEmbedServer(t, dim, 0, nil)
	defer srv.Close()
	cfg := baseCfg(dir, srv.URL, dim)
	if err := runPipeline(context.Background(), cfg, strings.NewReader(makeNDJSON(n))); err != nil {
		t.Fatalf("run initial: %v", err)
	}
	// Endpoint cassé : s'il est appelé, le run échoue → prouve qu'il ne l'est pas.
	cfg.endpoint = "http://127.0.0.1:1/broken"
	if err := runPipeline(context.Background(), cfg, strings.NewReader(makeNDJSON(n))); err != nil {
		t.Fatalf("reprise done devrait être un no-op, a: %v", err)
	}
}

// TestPipelineCompleteInterruptedFinalize couvre les deux fenêtres de kill PENDANT finalize
// (F1) : (W2) arène finale + ids final présents, manifest resté in_progress ; (W1) idem mais
// l'ids est encore en .tmp (rename non effectué). Dans les deux cas, une relance complète la
// finalisation SANS ré-embedder (endpoint cassé).
func TestPipelineCompleteInterruptedFinalize(t *testing.T) {
	dim := 8
	n := 200

	setup := func(t *testing.T) pipelineConfig {
		dir := t.TempDir()
		srv := fakeEmbedServer(t, dim, 0, nil)
		defer srv.Close()
		cfg := baseCfg(dir, srv.URL, dim)
		if err := runPipeline(context.Background(), cfg, strings.NewReader(makeNDJSON(n))); err != nil {
			t.Fatalf("run initial: %v", err)
		}
		// Rendre le manifest in_progress (simule un kill juste avant le passage à done).
		m, _ := readManifest(cfg.manifestPath)
		m.Status = statusInProgress
		if err := writeManifest(cfg.manifestPath, *m); err != nil {
			t.Fatal(err)
		}
		cfg.endpoint = "http://127.0.0.1:1/broken" // prouve qu'aucun embed n'est refait
		return cfg
	}

	t.Run("W2_ids_final_present", func(t *testing.T) {
		cfg := setup(t)
		if err := runPipeline(context.Background(), cfg, strings.NewReader(makeNDJSON(n))); err != nil {
			t.Fatalf("complétion W2: %v", err)
		}
		verifyComplete(t, cfg, n, dim)
	})

	t.Run("W1_ids_still_tmp", func(t *testing.T) {
		cfg := setup(t)
		// Défaire le rename ids : ids final → ids.tmp (état de la fenêtre W1).
		if err := os.Rename(cfg.idsPath, cfg.idsPath+".tmp"); err != nil {
			t.Fatal(err)
		}
		if err := runPipeline(context.Background(), cfg, strings.NewReader(makeNDJSON(n))); err != nil {
			t.Fatalf("complétion W1: %v", err)
		}
		verifyComplete(t, cfg, n, dim)
	})
}

func TestPipelineEmptyStreamFails(t *testing.T) {
	dir := t.TempDir()
	srv := fakeEmbedServer(t, 8, 0, nil)
	defer srv.Close()
	cfg := baseCfg(dir, srv.URL, 8)
	if err := runPipeline(context.Background(), cfg, strings.NewReader("")); err == nil {
		t.Fatal("un flux vide devrait échouer (aucun vecteur produit)")
	}
}
