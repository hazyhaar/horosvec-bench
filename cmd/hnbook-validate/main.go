// hnbook-validate est le runner de validation de bout en bout de l'index horosvec HNbook
// (vague W4 du méta-goal supervision HNbook). À partir d'un index SQLite vector-less adossé
// à son arène fp16, il exécute la recherche top-K de chaque requête d'un lot JSON, mesure la
// latence, relève le compteur de chargements SQL du rerank (attendu nul en mode arène), puis
// établit la VÉRITÉ FORCE BRUTE en UNE SEULE PASSE séquentielle sur l'arène (produit contre
// TOUTES les requêtes simultanément, top-K maintenu par requête, parallélisé par blocs bornés)
// afin de calculer le recouvrement (overlap@K) de la recherche approchée contre l'exact. Le
// verdict est écrit en JSON. Ce binaire est EXÉCUTÉ PAR L'ARCHITECTE sur le run 26,7 M —
// jamais par un subagent (run long rendu à l'architecte). L'arène n'est lue qu'en lecture
// seule ; aucune écriture n'est faite sur l'index.
//
// Réutilisation stricte de la lib horosvec (aucune réimplémentation de lecture d'arène) :
//   - horosvec.New(db, cfg{ArenaPath}) ouvre l'index en mode arène résidente ;
//   - Index.Search exécute la recherche approchée servie par l'arène ;
//   - Index.RerankSQLLoads relève le compteur de rerank SQL ;
//   - horosvec.OpenArenaReader + ArenaReader.VecInto décodent fp16→fp32 EXACTEMENT comme le
//     rerank du moteur (mêmes octets, même conversion) — la force brute mesure donc la vérité
//     du métrique réellement classé par le moteur.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/hazyhaar/horosvec"
	_ "modernc.org/sqlite"
)

// Seuils du verdict — constantes documentées, JAMAIS bricolables en argument de ligne de
// commande (un banc dont on peut abaisser le seuil pour le verdir ne prouve rien).
const (
	// overlapPassThreshold est le recouvrement moyen minimal exigé entre la recherche
	// approchée (Search) et la vérité force brute exacte, sur l'ensemble des requêtes.
	overlapPassThreshold = 0.90
	// expectRerankSQLLoads est le nombre de chargements SQL de rerank toléré : en mode arène,
	// le rerank est servi intégralement par l'arène fp16, donc zéro accès SQL ligne à ligne.
	expectRerankSQLLoads = 0
)

// queryInput est une requête du lot d'entrée {"queries":[{"qid":…,"text":…,"vector":[…]}]}.
// qid est conservé en JSON brut (nombre ou chaîne) et restitué verbatim dans le verdict, pour
// ne présumer d'aucun type d'identifiant côté producteur.
type queryInput struct {
	QID    json.RawMessage `json:"qid"`
	Text   string          `json:"text"`
	Vector []float64       `json:"vector"`
}

type queriesFile struct {
	Queries []queryInput `json:"queries"`
}

// perQuery est le verdict d'une requête.
type perQuery struct {
	QID       json.RawMessage `json:"qid"`
	TopKIDs   []string        `json:"topk_ids"`
	LatenceMS float64         `json:"latence_ms"`
	Overlap   float64         `json:"overlap"`
}

// aggregate est le verdict agrégé.
type aggregate struct {
	OverlapMoyen   float64 `json:"overlap_moyen"`
	P50MS          float64 `json:"p50_ms"`
	P99MS          float64 `json:"p99_ms"`
	RerankSQLLoads int64   `json:"rerank_sql_loads"`
	NonEmpty       int     `json:"non_empty"`
	Total          int     `json:"total"`
	Pass           bool    `json:"pass"`
}

// verdict est la structure racine écrite en JSON.
type verdict struct {
	IndexPath string     `json:"index_path"`
	ArenaPath string     `json:"arena_path"`
	TopK      int        `json:"topk"`
	Workers   int        `json:"workers"`
	Queries   []perQuery `json:"queries"`
	Aggregate aggregate  `json:"aggregate"`
	Seuils    struct {
		OverlapMoyenMin      float64 `json:"overlap_moyen_min"`
		RerankSQLLoadsAttend int64   `json:"rerank_sql_loads_attendu"`
	} `json:"seuils"`
}

// runConfig porte les entrées d'une exécution de validation, découplées des flags pour la
// testabilité (le test V4 appelle runValidation directement).
type runConfig struct {
	indexPath string
	arenaPath string
	idsPath   string
	topK      int
	workers   int
	queries   []queryInput
	report    *progress
}

func main() {
	indexPath := flag.String("index", "", "index SQLite vector-less (adossé à l'arène)")
	arenaPath := flag.String("arena", "", "arène fp16 HVARENA1 (lecture seule)")
	idsPath := flag.String("ids", "", "fichier d'ids (uint64 LE, rang = node_id ; défaut <arena>.ids)")
	queriesPath := flag.String("queries", "", "lot JSON de requêtes {queries:[{qid,text,vector}]}")
	topK := flag.Int("topk", 10, "nombre de voisins recherchés par requête")
	workers := flag.Int("workers", runtime.NumCPU(), "workers bornés de la passe force brute")
	outPath := flag.String("out", "", "verdict JSON de sortie (créé/écrasé)")
	progressPath := flag.String("progress", "", "fichier de progression appendu (défaut stderr seul)")
	flag.Parse()

	if *indexPath == "" || *arenaPath == "" || *queriesPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: hnbook-validate -index <db> -arena <path> -queries <json> -out <verdict.json> [-ids <path>] [-topk N] [-workers N] [-progress <path>]")
		os.Exit(2)
	}
	if *idsPath == "" {
		*idsPath = *arenaPath + ".ids"
	}

	report := newProgress(*progressPath)
	report.step("démarrage index=%s arena=%s queries=%s topk=%d workers=%d", *indexPath, *arenaPath, *queriesPath, *topK, *workers)

	queries, err := loadQueries(*queriesPath)
	if err != nil {
		fatal(report, "lecture requêtes: %v", err)
	}
	report.step("requêtes chargées n=%d", len(queries))

	v, err := runValidation(context.Background(), runConfig{
		indexPath: *indexPath,
		arenaPath: *arenaPath,
		idsPath:   *idsPath,
		topK:      *topK,
		workers:   *workers,
		queries:   queries,
		report:    report,
	})
	if err != nil {
		fatal(report, "validation: %v", err)
	}

	if err := writeVerdict(*outPath, v); err != nil {
		fatal(report, "écriture verdict: %v", err)
	}
	report.step("terminé pass=%v overlap_moyen=%.4f p50_ms=%.2f p99_ms=%.2f rerank_sql_loads=%d out=%s",
		v.Aggregate.Pass, v.Aggregate.OverlapMoyen, v.Aggregate.P50MS, v.Aggregate.P99MS, v.Aggregate.RerankSQLLoads, *outPath)
	if !v.Aggregate.Pass {
		os.Exit(1)
	}
}

// runValidation exécute la recherche approchée par requête (latence mesurée, top-K non vide
// asserté), relève le compteur de rerank SQL APRÈS toutes les recherches, puis établit la
// vérité force brute exacte en une passe et calcule les recouvrements et agrégats.
func runValidation(ctx context.Context, rc runConfig) (verdict, error) {
	if rc.topK <= 0 {
		return verdict{}, fmt.Errorf("topk invalide %d", rc.topK)
	}
	if len(rc.queries) == 0 {
		return verdict{}, fmt.Errorf("aucune requête")
	}
	if rc.workers <= 0 {
		rc.workers = 1
	}

	// L'index est ouvert en lecture-écriture (horosvec.New pose ses PRAGMAs et son schéma —
	// no-op sur un index déjà bâti — ce qui exclut le mode read-only), mais aucune écriture de
	// DONNÉE n'est faite : ni Insert, ni Build. L'arène, elle, n'est touchée qu'en lecture
	// (mmap ArenaReader). La validation ne mute donc pas le contenu de l'index.
	db, err := sql.Open("sqlite", rc.indexPath)
	if err != nil {
		return verdict{}, fmt.Errorf("open sqlite: %w", err)
	}
	defer db.Close()

	cfg := horosvec.DefaultConfig()
	cfg.ArenaPath = rc.arenaPath
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		return verdict{}, fmt.Errorf("horosvec new (mode arène): %w", err)
	}
	defer idx.Close()

	// Conversion des vecteurs de requête fp64→fp32 une seule fois (Search et force brute
	// travaillent en float32, comme l'arène décodée).
	qvecs := make([][]float32, len(rc.queries))
	for i, q := range rc.queries {
		v := make([]float32, len(q.Vector))
		for j, x := range q.Vector {
			v[j] = float32(x)
		}
		qvecs[i] = v
	}

	// Passe 1 — recherche approchée par requête (séquentielle), latences mesurées.
	searchIDs := make([][]string, len(rc.queries))
	latencies := make([]float64, len(rc.queries))
	nonEmpty := 0
	for i := range rc.queries {
		t0 := time.Now()
		res, err := idx.Search(ctx, qvecs[i], rc.topK)
		latencies[i] = float64(time.Since(t0).Microseconds()) / 1000.0
		if err != nil {
			return verdict{}, fmt.Errorf("search requête %d: %w", i, err)
		}
		ids := make([]string, len(res))
		for k, r := range res {
			ids[k] = string(r.ID)
		}
		searchIDs[i] = ids
		if len(ids) > 0 {
			nonEmpty++
		}
		if (i+1)%1000 == 0 {
			rc.report.step("recherche %d/%d", i+1, len(rc.queries))
		}
	}

	// Compteur de rerank SQL relevé APRÈS l'ensemble des recherches (cumul sur toutes les
	// requêtes) — jamais au milieu, sinon la mesure sous-estime.
	rerankSQL := idx.RerankSQLLoads()
	rc.report.step("recherches terminées non_vides=%d/%d rerank_sql_loads=%d", nonEmpty, len(rc.queries), rerankSQL)

	// Passe 2 — vérité force brute exacte en une seule passe sur l'arène.
	rc.report.step("force brute en cours (une passe, workers=%d)", rc.workers)
	bruteRanks, arenaCount, err := bruteForceTopK(ctx, rc.arenaPath, qvecs, rc.topK, rc.workers, rc.report)
	if err != nil {
		return verdict{}, fmt.Errorf("force brute: %w", err)
	}

	// Conversion rang→ext_id via le fichier d'ids (décodage identique à horosvec.readArenaIDs :
	// uint64 LE → décimal ASCII), pour comparer aux ext_id restitués par Search.
	idsArr, err := readIDs(rc.idsPath, arenaCount)
	if err != nil {
		return verdict{}, fmt.Errorf("ids: %w", err)
	}

	// Dénominateur du recouvrement = min(topK, N). Sur le run réel N ≫ topK ; la borne ne
	// mord que sur un corpus minuscule (test).
	denom := rc.topK
	if int64(denom) > arenaCount {
		denom = int(arenaCount)
	}

	pq := make([]perQuery, len(rc.queries))
	var overlapSum float64
	for i := range rc.queries {
		bruteIDs := make([]string, len(bruteRanks[i]))
		for j, rank := range bruteRanks[i] {
			bruteIDs[j] = idsArr[rank]
		}
		ov := overlap(searchIDs[i], bruteIDs, denom)
		overlapSum += ov
		pq[i] = perQuery{
			QID:       rc.queries[i].QID,
			TopKIDs:   searchIDs[i],
			LatenceMS: latencies[i],
			Overlap:   ov,
		}
	}

	overlapMoyen := overlapSum / float64(len(rc.queries))
	p50 := percentile(latencies, 50)
	p99 := percentile(latencies, 99)
	pass := overlapMoyen >= overlapPassThreshold &&
		nonEmpty == len(rc.queries) &&
		rerankSQL == expectRerankSQLLoads

	v := verdict{
		IndexPath: rc.indexPath,
		ArenaPath: rc.arenaPath,
		TopK:      rc.topK,
		Workers:   rc.workers,
		Queries:   pq,
		Aggregate: aggregate{
			OverlapMoyen:   overlapMoyen,
			P50MS:          p50,
			P99MS:          p99,
			RerankSQLLoads: rerankSQL,
			NonEmpty:       nonEmpty,
			Total:          len(rc.queries),
			Pass:           pass,
		},
	}
	v.Seuils.OverlapMoyenMin = overlapPassThreshold
	v.Seuils.RerankSQLLoadsAttend = expectRerankSQLLoads
	return v, nil
}

// overlap retourne |intersection(a,b)| / denom, borné à [0,1].
func overlap(a, b []string, denom int) float64 {
	if denom <= 0 {
		return 0
	}
	set := make(map[string]struct{}, len(a))
	for _, id := range a {
		set[id] = struct{}{}
	}
	inter := 0
	for _, id := range b {
		if _, ok := set[id]; ok {
			inter++
		}
	}
	return float64(inter) / float64(denom)
}

// percentile calcule le p-ième percentile (0-100) d'un échantillon de latences, par
// interpolation du plus proche rang sur une copie triée. Ne modifie pas l'entrée.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := make([]float64, len(xs))
	copy(cp, xs)
	sortFloat64(cp)
	if len(cp) == 1 {
		return cp[0]
	}
	idx := int((p / 100.0) * float64(len(cp)-1))
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func loadQueries(path string) ([]queryInput, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var qf queriesFile
	if err := json.Unmarshal(data, &qf); err != nil {
		return nil, err
	}
	if len(qf.Queries) == 0 {
		return nil, fmt.Errorf("lot vide (aucune requête)")
	}
	dim := len(qf.Queries[0].Vector)
	for i, q := range qf.Queries {
		if len(q.Vector) != dim {
			return nil, fmt.Errorf("requête %d : dimension %d != %d", i, len(q.Vector), dim)
		}
	}
	return qf.Queries, nil
}

func writeVerdict(path string, v verdict) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
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
			fmt.Fprintf(os.Stderr, "hnbook-validate: ouverture progression %s: %v\n", path, err)
		} else {
			p.f = f
		}
	}
	return p
}

func (p *progress) step(format string, args ...any) {
	line := fmt.Sprintf("%s hnbook-validate: %s", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, args...))
	fmt.Fprintln(os.Stderr, line)
	if p != nil && p.f != nil {
		fmt.Fprintln(p.f, line)
		_ = p.f.Sync()
	}
}

func fatal(p *progress, format string, args ...any) {
	p.step("ERREUR "+format, args...)
	os.Exit(1)
}
