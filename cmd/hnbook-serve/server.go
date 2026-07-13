package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hazyhaar/horosvec"
)

// maxPreviewURLParamLen borne la longueur du paramètre url de /api/preview (anti-abus), en
// cohérence avec previewMaxURLLen.
const maxPreviewURLParamLen = previewMaxURLLen

// maxQueryBytes borne la taille du texte de requête accepté (anti-abus). Au-delà, la requête
// est rejetée (400) avant tout travail d'embedding ou de recherche.
const maxQueryBytes = 512

// maxTopK plafonne le nombre de voisins qu'un appelant peut demander via le paramètre k
// (anti-abus : borne raisonnable, PAS illimité). La démo publique pagine côté navigateur sur
// cet ensemble ; au-delà, la valeur est écrêtée sans erreur.
const maxTopK = 100

// parseTopK résout le nombre de voisins demandés à partir du paramètre k de la requête. Absent,
// vide ou invalide (non entier, <= 0) => fallback (topK configuré au démarrage). Au-delà de
// maxTopK, la valeur est écrêtée à maxTopK. La forme de réponse reste inchangée : seul le nombre
// de résultats varie.
func parseTopK(raw string, fallback int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	if n > maxTopK {
		return maxTopK
	}
	return n
}

// searcher est l'interface de recherche consommée par le serveur, définie côté consommateur.
// L'implémentation de production est *horosvec.Index (mode arène) ; les tests fournissent un
// index réel de petite taille bâti par le harnais.
type searcher interface {
	Search(ctx context.Context, query []float32, topK int) ([]horosvec.Result, error)
}

// searchMeta expose les labels d'index interrogés/sautés (federatedSearcher).
type searchMeta interface {
	LastIndices() []string
	LastSkipped() []string
}

// server porte l'état immuable du service de démo (index en lecture seule, client d'embedding,
// table de titres optionnelle, limiteur de débit, page embarquée). Aucun état mutable de
// requête n'y est conservé : le service est sans état, seul le limiteur mute (sous son mutex).
type server struct {
	// idxHolder porte l'index de recherche publié atomiquement une fois le chargement terminé.
	// Il est nil pendant le préchauffage (démarrage : le port est lié tout de suite, l'index se
	// charge en tâche de fond). Les gestionnaires le lisent via index() sans course : l'index
	// n'est jamais accédé avant d'être prêt (aucun déréférencement de nil pendant le chargement).
	idxHolder atomic.Pointer[searcher]
	// loadErr porte un message d'erreur bref si le chargement de l'index a échoué (fail-loud). Non
	// nil => les routes rendent 503 avec ce message plutôt qu'une page de préchauffage éternelle.
	loadErr atomic.Pointer[string]
	// onClose porte la fermeture de l'index (arène + base sous-jacente), publiée atomiquement par
	// la goroutine de chargement et lue par le point d'assemblage à l'arrêt, sans course.
	onClose atomic.Pointer[func()]
	// warming est la page servie sur GET / tant que l'index n'est pas prêt (auto-rafraîchie).
	warming    []byte
	embed      *embedClient
	store      *titleStore
	lim        *ipLimiter
	reqTimeout time.Duration
	topK       int
	kRaw       int
	tau        float64
	html       []byte
	log        *slog.Logger
	// trustProxy autorise la lecture de X-Forwarded-For pour identifier l'appelant. Il n'est
	// activé QUE lorsque le service est réellement placé derrière un proxy de confiance (nginx),
	// faute de quoi l'en-tête est falsifiable et contournerait le limiteur de débit.
	trustProxy bool

	// prev porte le mandataire de prévisualisation Open Graph (client HTTP durci + cache). Il est
	// construit paresseusement à la première prévisualisation (previewer() sous prevOnce), ce qui
	// évite tout câblage supplémentaire au point d'assemblage du serveur.
	prev     *previewer
	prevOnce sync.Once
}

// setIndex publie l'index chargé : à partir de cet instant, les gestionnaires de recherche le
// voient prêt (bascule atomique du drapeau de disponibilité).
func (s *server) setIndex(idx searcher) {
	s.idxHolder.Store(&idx)
}

// index restitue l'index si le préchauffage est terminé, sinon (nil, false). Lecture atomique,
// sans course avec la goroutine de chargement.
func (s *server) index() (searcher, bool) {
	p := s.idxHolder.Load()
	if p == nil {
		return nil, false
	}
	return *p, true
}

// previewer restitue le mandataire de prévisualisation, construit une seule fois. Un previewer
// injecté (tests) est respecté ; sinon un previewer durci par défaut est bâti.
func (s *server) previewer() *previewer {
	s.prevOnce.Do(func() {
		if s.prev == nil {
			s.prev = newPreviewer()
		}
	})
	return s.prev
}

// threadRoot est l'en-tête story d'un fil dans la réponse JSON de /api/search.
type threadRoot struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Date  string `json:"date"`
	URL   string `json:"url"`
}

// threadHit est un commentaire (ou item) matché dans un fil.
type threadHit struct {
	ID           string  `json:"id"`
	Depth        int     `json:"depth"`
	TitleSnippet string  `json:"title_snippet,omitempty"`
	TextSnippet  string  `json:"text_snippet,omitempty"`
	D2           float64 `json:"d2"`
	URL          string  `json:"url"`
}

// itemResponse est le corps JSON rendu par GET /api/item.
type itemResponse struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Text  string `json:"text,omitempty"`
	Type  string `json:"type"`
	Date  string `json:"date"`
	URL   string `json:"url"`
}

// threadResult regroupe les hits d'un fil classé par score log-somme-exp.
type threadResult struct {
	Root  threadRoot  `json:"root"`
	Score float64     `json:"score"`
	Hits  []threadHit `json:"hits"`
}

// searchResponse est le corps JSON rendu par /api/search.
type searchResponse struct {
	Threads   []threadResult `json:"threads"`
	Freshness string         `json:"freshness"`
	Indices   []string       `json:"indices,omitempty"`
	Skipped   []string       `json:"skipped,omitempty"`
	LatencyMS float64        `json:"latency_ms"`
	EmbedMS   float64        `json:"embed_ms"`
}

// routes câble les gestionnaires HTTP sur un multiplexeur neuf.
func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/item", s.handleItem)
	mux.HandleFunc("GET /api/preview", s.handlePreview)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// handleIndex sert la page de recherche embarquée une fois l'index prêt. Pendant le
// préchauffage, il rend la page « préchauffage en cours » auto-rafraîchie (HTTP 200 : la page
// est statique, sans donnée distante). En cas d'échec de chargement, il rend un message bref en
// 503 plutôt qu'une promesse de disponibilité imminente.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, ok := s.index(); ok {
		_, _ = w.Write(s.html)
		return
	}
	if e := s.loadErr.Load(); e != nil {
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("<!doctype html><meta charset=utf-8><title>horosvec — unavailable</title>" +
			"<body style=\"background:#0d1117;color:#e6edf3;font-family:sans-serif;text-align:center;padding:4rem 1rem\">" +
			"<h1>Service temporarily unavailable</h1><p style=\"color:#8b949e\">Loading the index failed.</p></body>"))
		return
	}
	w.Header().Set("Retry-After", "5")
	_, _ = w.Write(s.warming)
}

// handleHealthz est ici une sonde de DISPONIBILITÉ (readiness), pas de simple vivacité : elle
// rend 200 uniquement lorsque l'index est chargé et que la recherche est réellement servie, 503
// tant que le préchauffage dure (ou en cas d'échec de chargement). C'est le signal honnête que
// lisent le rafraîchissement de page, la supervision et le proxy amont.
func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, ok := s.index(); ok {
		_, _ = w.Write([]byte("ok\n"))
		return
	}
	w.Header().Set("Retry-After", "5")
	w.WriteHeader(http.StatusServiceUnavailable)
	if e := s.loadErr.Load(); e != nil {
		_, _ = w.Write([]byte("error: " + *e + "\n"))
		return
	}
	_, _ = w.Write([]byte("warming\n"))
}

// warmingGate rend true (et a écrit la réponse) si l'index n'est pas encore prêt : les routes
// d'API répondent alors 503 + JSON {"status":"warming"} (ou l'erreur de chargement) avec un
// en-tête Retry-After, sans jamais toucher l'index. Il rend false quand l'index est prêt.
func (s *server) warmingGate(w http.ResponseWriter) bool {
	if _, ok := s.index(); ok {
		return false
	}
	w.Header().Set("Retry-After", "5")
	if e := s.loadErr.Load(); e != nil {
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "error": *e})
		return true
	}
	s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "warming"})
	return true
}

// handleSearch embarque la requête utilisateur : borne de taille, limitation de débit,
// embedding via le sidecar (fail-loud 503 si indisponible), recherche approchée sur l'index,
// puis rendu JSON. Toute valeur issue de l'utilisateur transite exclusivement en JSON encodé
// (échappement natif) ; le serveur ne concatène jamais d'entrée dans du HTML.
func (s *server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if s.warmingGate(w) {
		return
	}
	ip := clientIP(r, s.trustProxy)
	if !s.lim.allow(ip) {
		s.writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		s.writeError(w, http.StatusBadRequest, "missing q parameter")
		return
	}
	if len(q) > maxQueryBytes {
		s.writeError(w, http.StatusBadRequest, "query too long")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.reqTimeout)
	defer cancel()

	tEmbed := time.Now()
	vec, err := s.embed.embed(ctx, q)
	embedMS := float64(time.Since(tEmbed).Microseconds()) / 1000.0
	if err != nil {
		s.log.Error("embedding unavailable", "ip", ip, "err", err.Error())
		s.writeError(w, http.StatusServiceUnavailable, "embedding service unavailable")
		return
	}

	idx, ok := s.index()
	if !ok {
		// Bascule en préchauffage entre la garde d'entrée et ce point : réponse cohérente 503.
		w.Header().Set("Retry-After", "5")
		s.writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "warming"})
		return
	}

	kRaw := s.kRaw
	if kRaw <= 0 {
		kRaw = 300
	}
	tau := s.tau
	if tau <= 0 {
		tau = 0.1
	}

	tSearch := time.Now()
	res, err := idx.Search(ctx, vec, kRaw)
	if err != nil {
		s.log.Error("search failed", "ip", ip, "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "search unavailable")
		return
	}
	totalMS := embedMS + float64(time.Since(tSearch).Microseconds())/1000.0

	if s.store == nil {
		s.writeError(w, http.StatusServiceUnavailable, "title store unavailable")
		return
	}

	threads, err := s.buildThreads(ctx, res, tau)
	if err != nil {
		s.log.Error("thread build failed", "ip", ip, "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "search unavailable")
		return
	}

	if err := s.store.refreshFreshness(ctx); err != nil {
		s.log.Warn("freshness refresh failed", "ip", ip, "err", err.Error())
	}

	var indices, skipped []string
	if meta, ok := idx.(searchMeta); ok {
		indices = meta.LastIndices()
		skipped = meta.LastSkipped()
	}

	out := searchResponse{
		Threads:   threads,
		Freshness: formatHNDate(s.store.FreshnessTS()),
		Indices:   indices,
		Skipped:   skipped,
		LatencyMS: totalMS,
		EmbedMS:   embedMS,
	}

	s.log.Info("search", "ip", ip, "q_len", len(q), "threads", len(out.Threads),
		"latency_ms", totalMS, "embed_ms", embedMS)
	s.writeJSON(w, http.StatusOK, out)
}

type groupedHit struct {
	id   int64
	d2   float64
	item storeItem
}

// buildThreads regroupe les hits bruts par root_id, joint les titres racine, et classe.
func (s *server) buildThreads(ctx context.Context, res []horosvec.Result, tau float64) ([]threadResult, error) {
	if len(res) == 0 {
		return nil, nil
	}

	ids := make([]int64, 0, len(res))
	d2ByID := make(map[int64]float64, len(res))
	for _, rr := range res {
		id, ok := parseHitID(rr.ID)
		if !ok {
			continue
		}
		ids = append(ids, id)
		d2ByID[id] = rr.Score
	}
	items, err := s.store.fetchItemsByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}

	type threadAcc struct {
		rootID int64
		hits   []groupedHit
	}
	byRoot := make(map[int64]*threadAcc)
	rootOrder := make([]int64, 0)
	for _, id := range ids {
		it, ok := items[id]
		if !ok {
			continue
		}
		rootID := it.RootID
		if rootID == 0 {
			rootID = id
		}
		acc, ok := byRoot[rootID]
		if !ok {
			acc = &threadAcc{rootID: rootID}
			byRoot[rootID] = acc
			rootOrder = append(rootOrder, rootID)
		}
		acc.hits = append(acc.hits, groupedHit{id: id, d2: d2ByID[id], item: it})
	}

	rootIDs := make([]int64, len(rootOrder))
	copy(rootIDs, rootOrder)
	roots, err := s.store.fetchRootsByIDs(ctx, rootIDs)
	if err != nil {
		return nil, err
	}

	threads := make([]threadResult, 0, len(byRoot))
	for _, rootID := range rootOrder {
		acc := byRoot[rootID]
		root := roots[rootID]
		tr := threadResult{
			Root: threadRoot{
				ID:    strconv.FormatInt(rootID, 10),
				Title: truncateTitle(root.Title),
				Date:  formatHNDate(root.TS),
				URL:   hnItemURL(rootID),
			},
			Score: threadLogSumExpScore(acc.hits, tau),
			Hits:  []threadHit{},
		}
		sort.SliceStable(acc.hits, func(i, j int) bool {
			if acc.hits[i].item.Depth != acc.hits[j].item.Depth {
				return acc.hits[i].item.Depth < acc.hits[j].item.Depth
			}
			return acc.hits[i].id < acc.hits[j].id
		})
		for _, h := range acc.hits {
			if h.item.Depth == 0 && h.id == rootID {
				continue
			}
			hit := threadHit{
				ID:           strconv.FormatInt(h.id, 10),
				Depth:        h.item.Depth,
				TitleSnippet: truncateTitle(h.item.Title),
				D2:           h.d2,
				URL:          hnItemURL(h.id),
			}
			if h.item.Type == "comment" && h.item.Text != "" {
				hit.TextSnippet = truncateTextSnippet(h.item.Text)
			}
			tr.Hits = append(tr.Hits, hit)
		}
		threads = append(threads, tr)
	}

	sort.SliceStable(threads, func(i, j int) bool {
		if threads[i].Score != threads[j].Score {
			return threads[i].Score > threads[j].Score
		}
		return threads[i].Root.ID < threads[j].Root.ID
	})
	return threads, nil
}

// threadLogSumExpScore calcule tau*log(Σ exp(sim/tau)) avec sim=exp(-d²/tau).
func threadLogSumExpScore(hits []groupedHit, tau float64) float64 {
	var sum float64
	for _, h := range hits {
		sim := math.Exp(-h.d2 / tau)
		sum += math.Exp(sim / tau)
	}
	if sum <= 0 {
		return 0
	}
	return tau * math.Log(sum)
}

// handleItem rend les métadonnées et le texte d'un item depuis le magasin local.
func (s *server) handleItem(w http.ResponseWriter, r *http.Request) {
	if s.warmingGate(w) {
		return
	}
	ip := clientIP(r, s.trustProxy)
	if !s.lim.allow(ip) {
		s.writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	idStr := strings.TrimSpace(r.URL.Query().Get("id"))
	if idStr == "" {
		s.writeError(w, http.StatusBadRequest, "missing id parameter")
		return
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		s.writeError(w, http.StatusBadRequest, "invalid id parameter")
		return
	}

	if s.store == nil {
		s.writeError(w, http.StatusServiceUnavailable, "title store unavailable")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.reqTimeout)
	defer cancel()

	it, ok, err := s.store.itemByID(ctx, id)
	if err != nil {
		s.log.Error("item lookup failed", "ip", ip, "id", id, "err", err.Error())
		s.writeError(w, http.StatusInternalServerError, "item unavailable")
		return
	}
	if !ok {
		s.writeError(w, http.StatusNotFound, "item not found")
		return
	}

	s.writeJSON(w, http.StatusOK, itemResponse{
		ID:    strconv.FormatInt(it.ID, 10),
		Title: it.Title,
		Text:  it.Text,
		Type:  it.Type,
		Date:  formatHNDate(it.TS),
		URL:   hnItemURL(it.ID),
	})
}

// handlePreview rend un aperçu Open Graph de l'URL cible externe d'un item, que le navigateur ne
// peut pas récupérer lui-même (CORS). Il passe par le MÊME limiteur de débit par IP que
// /api/search. Le durcissement (anti-SSRF, bornes, non-HTML) vit dans preview.go ; ce handler
// borne l'entrée, sert le cache, et rend TOUJOURS un JSON en 200 (champs vides + error en cas
// d'échec) pour que le panneau se dégrade gracieusement, jamais un 500.
func (s *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	if s.warmingGate(w) {
		return
	}
	ip := clientIP(r, s.trustProxy)
	if !s.lim.allow(ip) {
		s.writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}

	raw := strings.TrimSpace(r.URL.Query().Get("url"))
	if raw == "" {
		s.writeJSON(w, http.StatusOK, previewResult{Error: "missing url parameter"})
		return
	}
	if len(raw) > maxPreviewURLParamLen {
		s.writeJSON(w, http.StatusOK, previewResult{URL: raw[:64], Error: "url too long"})
		return
	}

	p := s.previewer()
	if res, ok := p.lookup(raw); ok {
		s.writeJSON(w, http.StatusOK, res)
		return
	}

	res := p.fetch(r.Context(), raw)
	p.store(raw, res)

	if res.Error != "" {
		s.log.Info("degraded preview", "ip", ip, "err", res.Error)
	}
	s.writeJSON(w, http.StatusOK, res)
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
