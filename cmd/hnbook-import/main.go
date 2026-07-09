// hnbook-import construit un index horosvec COMPLET à partir d'une arène fp16
// EXISTANTE (HVARENA1, manifest done), de son fichier d'ids et d'une adjacence
// plate u32 produite en amont par un build GPU CAGRA — le runner de la vague
// d'import (W2) du méta-goal supervision HNbook. Il n'exécute JAMAIS de build de
// graphe : l'adjacence est lue (mmap) et l'index encodé/persisté par la voie
// horosvec.ImportAdjacency (rotation, RaBitQ, normes, médoïde, SQLite vector-less,
// plan chaud, garde de normalisation). Prévu pour le run 26,7 M — jamais lancé par
// un subagent (run long rendu à l'architecte).
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hazyhaar/horosvec"
	"github.com/hazyhaar/horosvec-bench/pkg/storagemedium"
	_ "modernc.org/sqlite"
)

// warnIfRotational émet un avertissement FORT si le chemin donné réside sur un support
// rotationnel : la latence d'accès à l'arène/l'index y dépasse le SSD d'un facteur 100 à
// 370 (campagne 2026-07). Fail-soft : "unknown" ou "ssd" ne dit rien.
func warnIfRotational(log *slog.Logger, role, path string) {
	if storagemedium.Resolve(path).Medium == storagemedium.Rotational {
		log.Warn("support de stockage rotationnel détecté : latence ×100-370 mesurée sur ce support — cf campagne 2026-07",
			"role", role, "path", path)
	}
}

// adjMeta reflète cagra_adjacency.meta.json (clés produites par le build GPU). degree est le
// nombre de voisins par ligne d'adjacence ; n le nombre de nœuds (journalisé ici à titre
// indicatif — la cohérence effective count/degree/taille est garantie en aval par les gardes
// fail-loud de horosvec.ImportAdjacency : compte d'ids == count d'arène et taille d'adjacence
// == count×degree×4).
type adjMeta struct {
	N      int64  `json:"n"`
	Degree int    `json:"degree"`
	Metric string `json:"metric"`
}

func main() {
	arenaPath := flag.String("arena", "", "arène fp16 HVARENA1 (complète, manifest done)")
	idsPath := flag.String("ids", "", "fichier d'ids (uint64 LE, rang = node_id ; défaut <arena>.ids)")
	adjPath := flag.String("adjacency", "", "adjacence plate u32 LE (N×degree, ligne i = node_id i)")
	metaPath := flag.String("meta", "", "méta JSON de l'adjacence (degree/n ; défaut <adjacency>.meta.json)")
	degreeFlag := flag.Int("degree", 0, "voisins par ligne (0 = lu depuis la méta ; sinon override)")
	outPath := flag.String("out", "", "index SQLite de sortie (créé/écrasé)")
	progressPath := flag.String("progress", "", "fichier de progression appendu (défaut stderr seul)")
	flag.Parse()
	if *arenaPath == "" || *adjPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: hnbook-import -arena <path> -adjacency <path.u32> -out <index.db> [-ids <path>] [-meta <path.json>] [-degree N] [-progress <path>]")
		os.Exit(2)
	}
	if *idsPath == "" {
		*idsPath = *arenaPath + ".ids"
	}
	if *metaPath == "" {
		*metaPath = *adjPath + ".meta.json"
	}

	report := newProgress(*progressPath)
	report.step("démarrage arena=%s ids=%s adjacency=%s out=%s", *arenaPath, *idsPath, *adjPath, *outPath)

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	warnIfRotational(log, "arène", *arenaPath)
	warnIfRotational(log, "index", *outPath)

	degree := *degreeFlag
	if meta, err := readMeta(*metaPath); err == nil {
		report.step("méta lue degree=%d n=%d metric=%s", meta.Degree, meta.N, meta.Metric)
		if degree == 0 {
			degree = meta.Degree
		}
	} else if degree == 0 {
		fatal(report, "méta illisible (%v) et -degree non fourni : impossible de déterminer le degré", err)
	}
	if degree <= 0 {
		fatal(report, "degré invalide %d", degree)
	}

	db, err := sql.Open("sqlite", *outPath)
	if err != nil {
		fatal(report, "open sqlite: %v", err)
	}
	defer db.Close()

	cfg := horosvec.DefaultConfig()
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		fatal(report, "horosvec new: %v", err)
	}

	report.step("import en cours (degree=%d, MaxDegree=%d)", degree, cfg.MaxDegree)
	t0 := time.Now()
	if err := idx.ImportAdjacency(context.Background(), *arenaPath, *idsPath, *adjPath, degree); err != nil {
		fatal(report, "import adjacency: %v", err)
	}
	report.step("terminé import_s=%.1f out=%s", time.Since(t0).Seconds(), *outPath)
}

// progress appende chaque étape franchie à stderr et, si configuré, à un fichier (une ligne
// horodatée par étape) : un run tué se lit au fichier, sans archéologie de mtimes.
type progress struct {
	f *os.File
}

func newProgress(path string) *progress {
	p := &progress{}
	if path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hnbook-import: ouverture progression %s: %v\n", path, err)
		} else {
			p.f = f
		}
	}
	return p
}

func (p *progress) step(format string, args ...any) {
	line := fmt.Sprintf("%s hnbook-import: %s", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
	fmt.Fprintln(os.Stderr, line)
	if p.f != nil {
		fmt.Fprintln(p.f, line)
		_ = p.f.Sync()
	}
}

func readMeta(path string) (adjMeta, error) {
	var m adjMeta
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, err
	}
	return m, nil
}

func fatal(p *progress, format string, args ...any) {
	p.step("ERREUR "+format, args...)
	os.Exit(1)
}
