// hnbook-serve est le service de démonstration publique de l'index horosvec HNbook : un binaire
// unique qui sert une page de recherche sémantique et une API JSON, destiné à
// horosvec.hazyhaar.fr derrière nginx (VPS CPU seul, sans GPU). L'index vector-less et son
// arène fp16 sont ouverts en LECTURE SEULE via la bibliothèque horosvec (aucune réimplémentation
// de lecture d'arène : mêmes appels que cmd/hnbook-validate). L'embedding de la requête est
// délégué à un sidecar Python local (embed_sidecar.py) interrogé en HTTP, qui reproduit à
// l'identique le pipeline de référence (pooling dernier token, normalisation, troncature
// Matryoshka 512, renormalisation, variante sans EOS). Le serveur est sans état ; l'anti-abus
// (limitation de débit par IP, bornes de taille, expiration) est stdlib pure.
package main

import (
	"context"
	"database/sql"
	_ "embed"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hazyhaar/horosvec"
	"github.com/hazyhaar/horosvec-bench/pkg/storagemedium"
	_ "modernc.org/sqlite"
)

//go:embed index.html
var indexHTML []byte

//go:embed warming.html
var warmingHTML []byte

// loadErrorGrace est le délai laissé après un échec de chargement de l'index avant que le
// process ne sorte en erreur : le temps qu'un visiteur voie la page d'indisponibilité et que la
// supervision lise un /healthz en 503, sans jamais rester en préchauffage éternel silencieux.
const loadErrorGrace = 30 * time.Second

// warnIfRotational émet un avertissement FORT si le chemin réside sur un support
// rotationnel (latence ×100-370 vs SSD, campagne 2026-07). Fail-soft.
func warnIfRotational(log *slog.Logger, role, path string) {
	if storagemedium.Resolve(path).Medium == storagemedium.Rotational {
		log.Warn("support de stockage rotationnel détecté : latence ×100-370 mesurée sur ce support — cf campagne 2026-07",
			"role", role, "path", path)
	}
}

func main() {
	indexPath := flag.String("index", "", "index SQLite vector-less (adossé à l'arène, lecture seule)")
	arenaPath := flag.String("arena", "", "arène fp16 HVARENA1 (lecture seule)")
	idsPath := flag.String("ids", "", "fichier d'ids optionnel (uint64 LE ; non requis, Search rend l'ext_id)")
	titlesPath := flag.String("titles", "", "fichier optionnel de titres id<TAB>titre (chargé en map si présent)")
	embedURL := flag.String("embed-url", "http://127.0.0.1:8471/embed", "endpoint HTTP du sidecar d'embedding")
	addr := flag.String("addr", "127.0.0.1:8472", "adresse d'écoute du serveur HTTP")
	topK := flag.Int("topk", 10, "nombre de voisins rendus par requête")
	trustProxy := flag.Bool("trust-proxy", false, "lire X-Forwarded-For (à n'activer QUE derrière un proxy de confiance, ex. nginx)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	if *indexPath == "" || *arenaPath == "" {
		fmt.Fprintln(os.Stderr, "usage: hnbook-serve -index <db> -arena <path> [-ids <path>] [-titles <path>] [-embed-url <url>] [-addr <hostport>] [-topk N]")
		os.Exit(2)
	}
	_ = *idsPath // Exposé pour symétrie documentaire avec hnbook-validate ; Search restitue déjà l'ext_id HN.
	if *topK <= 0 {
		fmt.Fprintln(os.Stderr, "hnbook-serve: -topk doit être strictement positif")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	warnIfRotational(log, "arène", *arenaPath)
	warnIfRotational(log, "index", *indexPath)

	// Les titres sont un simple chargement de fichier (rapide) et n'entrent pas dans la fenêtre
	// coûteuse de ~90 s : ils restent synchrones, avant la liaison du port.
	titles, err := loadTitles(*titlesPath)
	if err != nil {
		log.Error("chargement titres", "path", *titlesPath, "err", err.Error())
		os.Exit(1)
	}
	if *titlesPath == "" {
		log.Info("aucun fichier de titres : la page affiche l'id HN et son lien")
	} else {
		log.Info("titres chargés", "n", len(titles))
	}

	srv := &server{
		embed:      &embedClient{url: *embedURL, hc: &http.Client{Timeout: 10 * time.Second}},
		titles:     titles,
		lim:        newIPLimiter(ctx, 1.0, 5.0),
		reqTimeout: 10 * time.Second,
		topK:       *topK,
		html:       indexHTML,
		warming:    warmingHTML,
		log:        log,
		trustProxy: *trustProxy,
	}

	// Le port est lié IMMÉDIATEMENT, avant tout chargement d'index : dès cet instant le proxy
	// amont obtient une réponse (page de préchauffage) au lieu d'un 502 pendant ~90 s.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("liaison du port", "addr", *addr, "err", err.Error())
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Handler:           http.MaxBytesHandler(srv.routes(), 4096),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Chargement de l'index en tâche de fond : le service sert déjà la page de préchauffage
	// pendant ce temps. Succès -> publication atomique de l'index (bascule à « prêt »). Échec ->
	// état d'erreur fail-loud, puis sortie non-zéro après un délai de grâce (jamais de
	// préchauffage éternel silencieux).
	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("panique dans la goroutine de chargement de l'index", "panic", p)
				msg := "erreur interne au chargement de l'index"
				srv.loadErr.Store(&msg)
			}
		}()
		idx, dbCloser, err := openIndex(*indexPath, *arenaPath)
		if err != nil {
			log.Error("chargement de l'index échoué (fail-loud)", "err", err.Error())
			msg := "chargement de l'index impossible"
			srv.loadErr.Store(&msg)
			time.Sleep(loadErrorGrace)
			log.Error("arrêt du process après échec de chargement de l'index", "grace", loadErrorGrace.String())
			os.Exit(1)
		}
		closeIdx := func() { _ = idx.Close(); dbCloser() }
		srv.onClose.Store(&closeIdx)
		srv.setIndex(idx)
		log.Info("index chargé, recherche disponible", "index", *indexPath, "arena", *arenaPath)
	}()

	go func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("panique dans la goroutine d'arrêt", "panic", p)
			}
		}()
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Info("hnbook-serve à l'écoute (préchauffage de l'index en cours)", "addr", *addr,
		"index", *indexPath, "arena", *arenaPath, "embed_url", *embedURL, "topk", *topK)
	if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Error("serveur HTTP", "err", err.Error())
		os.Exit(1)
	}
	if c := srv.onClose.Load(); c != nil {
		(*c)()
	}
	log.Info("hnbook-serve arrêté proprement")
}

// openIndex ouvre l'index horosvec en mode arène résidente (lecture seule des données), selon
// le chemin exact de cmd/hnbook-validate. Retourne l'index, une fermeture de la base sous-
// jacente, et une éventuelle erreur.
func openIndex(indexPath, arenaPath string) (*horosvec.Index, func(), error) {
	db, err := sql.Open("sqlite", indexPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite: %w", err)
	}
	cfg := horosvec.DefaultConfig()
	cfg.ArenaPath = arenaPath
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("horosvec new (mode arène): %w", err)
	}
	return idx, func() { _ = db.Close() }, nil
}
