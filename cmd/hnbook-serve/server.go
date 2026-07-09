package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hazyhaar/horosvec"
)

// maxQueryBytes borne la taille du texte de requête accepté (anti-abus). Au-delà, la requête
// est rejetée (400) avant tout travail d'embedding ou de recherche.
const maxQueryBytes = 512

// searcher est l'interface de recherche consommée par le serveur, définie côté consommateur.
// L'implémentation de production est *horosvec.Index (mode arène) ; les tests fournissent un
// index réel de petite taille bâti par le harnais.
type searcher interface {
	Search(ctx context.Context, query []float32, topK int) ([]horosvec.Result, error)
}

// server porte l'état immuable du service de démo (index en lecture seule, client d'embedding,
// table de titres optionnelle, limiteur de débit, page embarquée). Aucun état mutable de
// requête n'y est conservé : le service est sans état, seul le limiteur mute (sous son mutex).
type server struct {
	idx        searcher
	embed      *embedClient
	titles     map[string]string
	lim        *ipLimiter
	reqTimeout time.Duration
	topK       int
	html       []byte
	log        *slog.Logger
	// trustProxy autorise la lecture de X-Forwarded-For pour identifier l'appelant. Il n'est
	// activé QUE lorsque le service est réellement placé derrière un proxy de confiance (nginx),
	// faute de quoi l'en-tête est falsifiable et contournerait le limiteur de débit.
	trustProxy bool
}

// searchResult est une entrée de la réponse JSON de /api/search.
type searchResult struct {
	ID           string  `json:"id"`
	Score        float64 `json:"score"`
	TitleSnippet string  `json:"title_snippet,omitempty"`
}

// searchResponse est le corps JSON rendu par /api/search.
type searchResponse struct {
	Results   []searchResult `json:"results"`
	LatencyMS float64        `json:"latency_ms"`
	EmbedMS   float64        `json:"embed_ms"`
}

// routes câble les gestionnaires HTTP sur un multiplexeur neuf.
func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// handleIndex sert la page de recherche embarquée.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(s.html)
}

// handleHealthz est la sonde de vivacité du serveur (n'engage pas le sidecar).
func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// handleSearch embarque la requête utilisateur : borne de taille, limitation de débit,
// embedding via le sidecar (fail-loud 503 si indisponible), recherche approchée sur l'index,
// puis rendu JSON. Toute valeur issue de l'utilisateur transite exclusivement en JSON encodé
// (échappement natif) ; le serveur ne concatène jamais d'entrée dans du HTML.
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r, s.trustProxy)
	if !s.lim.allow(ip) {
		s.writeError(w, http.StatusTooManyRequests, "trop de requêtes")
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		s.writeError(w, http.StatusBadRequest, "paramètre q manquant")
		return
	}
	if len(q) > maxQueryBytes {
		s.writeError(w, http.StatusBadRequest, "requête trop longue")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.reqTimeout)
	defer cancel()

	tEmbed := time.Now()
	vec, err := s.embed.embed(ctx, q)
	embedMS := float64(time.Since(tEmbed).Microseconds()) / 1000.0
	if err != nil {
		s.log.Error("embedding indisponible", "ip", ip, "err", err.Error())
		s.writeError(w, http.StatusServiceUnavailable, "service d'embedding indisponible")
		return
	}

	tSearch := time.Now()
	res, err := s.idx.Search(ctx, vec, s.topK)
	if err != nil {
		s.log.Error("recherche échouée", "ip", ip, "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "recherche indisponible")
		return
	}
	totalMS := embedMS + float64(time.Since(tSearch).Microseconds())/1000.0

	out := searchResponse{
		Results:   make([]searchResult, 0, len(res)),
		LatencyMS: totalMS,
		EmbedMS:   embedMS,
	}
	for _, rr := range res {
		id := string(rr.ID)
		sr := searchResult{ID: id, Score: rr.Score}
		if s.titles != nil {
			if t, ok := s.titles[id]; ok {
				sr.TitleSnippet = t
			}
		}
		out.Results = append(out.Results, sr)
	}

	s.log.Info("recherche", "ip", ip, "q_len", len(q), "results", len(out.Results),
		"latency_ms", totalMS, "embed_ms", embedMS)
	s.writeJSON(w, http.StatusOK, out)
}

func (s *server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// clientIP restitue l'adresse de l'appelant servant de clé au limiteur de débit.
//
// Par défaut (trustProxy faux, service exposé en direct), seule l'adresse de la connexion
// (RemoteAddr) fait foi : X-Forwarded-For est ignoré car un client peut le forger et créer une
// clé de seau neuve à chaque requête, ce qui contournerait le limiteur et gonflerait la table
// des seaux (épuisement mémoire). Lorsque le service est placé derrière un proxy de confiance
// (nginx, option -trust-proxy), c'est le maillon LE PLUS À DROITE de X-Forwarded-For qui porte
// l'IP réelle : nginx (directive $proxy_add_x_forwarded_for) appose l'adresse du pair immédiat
// APRÈS toute valeur reçue, si bien qu'une valeur falsifiée par le client reste à gauche et
// n'est jamais retenue.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
