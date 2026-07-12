// proto-semantic-harness — pré-passe hybride BM25 + dense sur 6 shards calendaires HN 2007-03..08.
package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hazyhaar/horosvec"

	_ "modernc.org/sqlite"
)

const (
	textDBPath = "/devhoros/horosvec-bench/data/hn_text_1m.db"
	arenaPath  = "/inference/hnbook/bench_final/prefix1m.arena"
	workDir    = "/devhoros/horosvec-bench/data/semantic-harness"
	nQueries   = 200
	topK       = 10
	bm25Limit  = 200
	querySeedA = 42
	querySeedB = 7
)

type shardSpec struct {
	label string
	start int64
	end   int64
}

var shardMonths = []shardSpec{
	{"2007-03", 1172707200, 1175385600},
	{"2007-04", 1175385600, 1177977600},
	{"2007-05", 1177977600, 1180656000},
	{"2007-06", 1180656000, 1183248000},
	{"2007-07", 1183248000, 1185926400},
	{"2007-08", 1185926400, 1188604800},
}

type itemRow struct {
	ID    int64
	Title string
	Text  string
}

type shardData struct {
	spec   shardSpec
	items  []itemRow
	dbPath string
	idx    *horosvec.Index
	db     *sql.DB
}

type queryItem struct {
	ID    int64
	Title string
	Vec   []float32
}

type scoredID struct {
	id    int64
	score float64
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "proto-semantic-harness: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	textDB, err := sql.Open("sqlite", textDBPath+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer textDB.Close()

	arena, err := openArena(arenaPath)
	if err != nil {
		return err
	}
	defer arena.Close()

	// --- Étape 1 : schéma shards ---
	shards, unionItems, volumes, err := loadShards(textDB, arena)
	if err != nil {
		return err
	}
	appendRapport("## Étape 1 — Schéma shards\n\n```\n" + volumes + "\n```\n\nUnion corpus (title!='' OR text!='') : **" + fmt.Sprintf("%d", len(unionItems)) + "** items.\n")

	// --- Étape 2 : build FTS5 ---
	ftsLog, err := buildFTS5(shards)
	if err != nil {
		return err
	}
	appendRapport("## Étape 2 — Build FTS5\n\n" + ftsLog + "\n")

	// --- Étape 3 : build index horosvec ---
	idxLog, err := buildHorosvecIndexes(shards, arena)
	if err != nil {
		return err
	}
	appendRapport("## Étape 3 — Build index horosvec\n\n" + idxLog + "\n")

	queries, err := selectQueries(textDB, arena)
	if err != nil {
		return err
	}

	unionVecs, err := loadUnionVectors(arena, unionItems)
	if err != nil {
		return err
	}

	// --- Étape 4 : vérité terrain ---
	gt, gtLog, err := computeGroundTruth(queries, unionItems, unionVecs)
	if err != nil {
		return err
	}
	appendRapport("## Étape 4 — Vérité terrain (brute-force L2)\n\n" + gtLog + "\n")

	// --- Étape 5 : run benchmark ---
	results, runLog, err := runBenchmark(shards, queries, gt, len(unionItems))
	if err != nil {
		return err
	}
	appendRapport("## Étape 5 — Run hybride vs dense\n\n" + runLog + "\n")

	seuil := results.hybridRecall >= results.denseRecall-0.005 && results.hybridP95 <= 1.2*results.denseP95
	verdict := "SUCCÈS"
	if !seuil {
		verdict = "ÉCHEC — hybride ne atteint pas le seuil (recall@10 ≥ dense−0,005 ET p95 ≤ 1,2× dense)"
	}

	out := map[string]any{
		"fichiers_crees": []string{
			"cmd/proto-semantic-harness/main.go",
			"cmd/proto-semantic-harness/arena.go",
		},
		"build_vert":             true,
		"volumes_shards":         shardVolumeList(shards),
		"tableau_recall_latence": results.tableau,
		"decomposition":          results.decomposition,
		"seuil_atteint":          seuil,
		"verdict":                verdict,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// loadShards charge les items des 6 mois et FILTRE ceux sans vecteur d'arène
// (le pipeline d'embedding a sauté les items morts : le mapping .ids fait foi,
// un item hors mapping n'existe ni pour le dense ni pour la vérité terrain —
// il resterait sinon un candidat BM25 sans coordonnées, biaisant l'hybride).
func loadShards(textDB *sql.DB, arena *arenaReader) ([]*shardData, []itemRow, string, error) {
	var shards []*shardData
	var union []itemRow
	var b strings.Builder
	b.WriteString("shard       start_ts    end_ts      volume\n")
	b.WriteString("----------  ----------  ----------  ------\n")

	for _, spec := range shardMonths {
		rows, err := textDB.Query(
			`SELECT id, title, text FROM item
			 WHERE (title!='' OR text!='') AND ts>=? AND ts<? ORDER BY id`,
			spec.start, spec.end,
		)
		if err != nil {
			return nil, nil, "", err
		}
		var items []itemRow
		for rows.Next() {
			var it itemRow
			if err := rows.Scan(&it.ID, &it.Title, &it.Text); err != nil {
				rows.Close()
				return nil, nil, "", err
			}
			if _, ok := arena.rank[it.ID]; !ok {
				continue // item sans vecteur (sauté à l'embedding)
			}
			items = append(items, it)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, nil, "", err
		}
		fmt.Fprintf(&b, "%-10s  %-10d  %-10d  %6d\n", spec.label, spec.start, spec.end, len(items))
		shards = append(shards, &shardData{
			spec:   spec,
			items:  items,
			dbPath: filepath.Join(workDir, "shard_"+spec.label+".db"),
		})
		union = append(union, items...)
	}
	return shards, union, b.String(), nil
}

func buildFTS5(shards []*shardData) (string, error) {
	var b strings.Builder
	for _, sh := range shards {
		_ = os.Remove(sh.dbPath)
		db, err := sql.Open("sqlite", sh.dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			return "", err
		}
		if _, err := db.Exec(`CREATE VIRTUAL TABLE docs USING fts5(title, text, tokenize='unicode61')`); err != nil {
			_ = db.Close()
			return "", fmt.Errorf("fts5 %s: %w", sh.spec.label, err)
		}
		tx, err := db.Begin()
		if err != nil {
			_ = db.Close()
			return "", err
		}
		st, err := tx.Prepare(`INSERT INTO docs(rowid, title, text) VALUES (?, ?, ?)`)
		if err != nil {
			_ = tx.Rollback()
			_ = db.Close()
			return "", err
		}
		for _, it := range sh.items {
			if _, err := st.Exec(it.ID, it.Title, it.Text); err != nil {
				_ = st.Close()
				_ = tx.Rollback()
				_ = db.Close()
				return "", err
			}
		}
		_ = st.Close()
		if err := tx.Commit(); err != nil {
			_ = db.Close()
			return "", err
		}
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM docs`).Scan(&n); err != nil {
			_ = db.Close()
			return "", err
		}
		_ = db.Close()
		fmt.Fprintf(&b, "- shard **%s** : FTS5 `docs` → %d lignes\n", sh.spec.label, n)
	}
	return b.String(), nil
}

type shardVecIter struct {
	arena *arenaReader
	items []itemRow
	i     int
}

func (it *shardVecIter) Next() (id []byte, vec []float32, ok bool) {
	if it.i >= len(it.items) {
		return nil, nil, false
	}
	item := it.items[it.i]
	it.i++
	vec, err := it.arena.ReadVec(int(item.ID))
	if err != nil {
		panic(err)
	}
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], uint64(item.ID))
	return buf[:], vec, true
}

func (it *shardVecIter) Reset() error {
	it.i = 0
	return nil
}

func buildHorosvecIndexes(shards []*shardData, arena *arenaReader) (string, error) {
	var b strings.Builder
	for _, sh := range shards {
		db, err := sql.Open("sqlite", sh.dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
		if err != nil {
			return "", err
		}
		cfg := horosvec.DefaultConfig()
		idx, err := horosvec.New(db, cfg)
		if err != nil {
			_ = db.Close()
			return "", fmt.Errorf("horosvec new %s: %w", sh.spec.label, err)
		}
		t0 := time.Now()
		iter := &shardVecIter{arena: arena, items: sh.items}
		if err := idx.Build(context.Background(), iter); err != nil {
			_ = idx.Close()
			_ = db.Close()
			return "", fmt.Errorf("horosvec build %s: %w", sh.spec.label, err)
		}
		elapsed := time.Since(t0)
		sh.db = db
		sh.idx = idx
		fmt.Fprintf(&b, "- shard **%s** : horosvec db-blob, %d vecteurs, build %.2fs\n",
			sh.spec.label, len(sh.items), elapsed.Seconds())
	}
	return b.String(), nil
}

func selectQueries(textDB *sql.DB, arena *arenaReader) ([]queryItem, error) {
	rows, err := textDB.Query(
		`SELECT id, title FROM item
		 WHERE type='story' AND title!='' AND ts>=? AND ts<? ORDER BY id`,
		shardMonths[0].start, shardMonths[len(shardMonths)-1].end,
	)
	if err != nil {
		return nil, err
	}
	var pool []queryItem
	for rows.Next() {
		var q queryItem
		if err := rows.Scan(&q.ID, &q.Title); err != nil {
			rows.Close()
			return nil, err
		}
		if _, ok := arena.rank[q.ID]; !ok {
			continue // story sans vecteur (sautée à l'embedding)
		}
		pool = append(pool, q)
	}
	rows.Close()
	if len(pool) < nQueries {
		return nil, fmt.Errorf("pool requêtes %d < %d", len(pool), nQueries)
	}

	indices := make([]int, len(pool))
	for i := range indices {
		indices[i] = i
	}
	rng := rand.New(rand.NewPCG(querySeedA, querySeedB))
	for i := 0; i < nQueries; i++ {
		j := i + rng.IntN(len(pool)-i)
		indices[i], indices[j] = indices[j], indices[i]
	}

	queries := make([]queryItem, nQueries)
	for i := 0; i < nQueries; i++ {
		q := pool[indices[i]]
		vec, err := arena.ReadVec(int(q.ID))
		if err != nil {
			return nil, err
		}
		q.Vec = vec
		queries[i] = q
	}
	return queries, nil
}

func loadUnionVectors(arena *arenaReader, items []itemRow) (map[int64][]float32, error) {
	m := make(map[int64][]float32, len(items))
	for _, it := range items {
		vec, err := arena.ReadVec(int(it.ID))
		if err != nil {
			return nil, err
		}
		m[it.ID] = vec
	}
	return m, nil
}

func computeGroundTruth(queries []queryItem, union []itemRow, vecs map[int64][]float32) ([][]int64, string, error) {
	gt := make([][]int64, len(queries))
	for qi, q := range queries {
		var scores []scoredID
		for _, it := range union {
			if it.ID == q.ID {
				continue
			}
			scores = append(scores, scoredID{id: it.ID, score: l2Squared(q.Vec, vecs[it.ID])})
		}
		sort.Slice(scores, func(i, j int) bool {
			if scores[i].score == scores[j].score {
				return scores[i].id < scores[j].id
			}
			return scores[i].score < scores[j].score
		})
		k := topK
		if k > len(scores) {
			k = len(scores)
		}
		row := make([]int64, k)
		for i := 0; i < k; i++ {
			row[i] = scores[i].id
		}
		gt[qi] = row
	}
	log := fmt.Sprintf("GT brute-force L2 fp32 : %d requêtes × union %d items → top-%d par requête (self-match exclu).",
		len(queries), len(union), topK)
	return gt, log, nil
}

func sanitizeFTSQuery(title string) string {
	var parts []string
	for _, tok := range strings.Fields(title) {
		tok = strings.ReplaceAll(tok, `"`, `""`)
		if tok != "" {
			parts = append(parts, `"`+tok+`"`)
		}
	}
	if len(parts) == 0 {
		return `""`
	}
	// OR entre tokens du titre : requête = titre en BM25 (bag-of-words), pas AND strict.
	return strings.Join(parts, " OR ")
}

func bm25Search(db *sql.DB, title string, excludeID int64, limit int) ([]int64, error) {
	q := sanitizeFTSQuery(title)
	rows, err := db.Query(
		`SELECT rowid, bm25(docs) AS rank FROM docs WHERE docs MATCH ? ORDER BY rank LIMIT ?`,
		q, limit+1,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		var rank float64
		if err := rows.Scan(&id, &rank); err != nil {
			return nil, err
		}
		if id == excludeID {
			continue
		}
		ids = append(ids, id)
		if len(ids) >= limit {
			break
		}
	}
	return ids, rows.Err()
}

func extIDToInt64(extID []byte) int64 {
	if len(extID) == 8 {
		return int64(binary.LittleEndian.Uint64(extID))
	}
	var n int64
	for _, c := range extID {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	return n
}

func denseSearch(shards []*shardData, q queryItem) ([]int64, error) {
	var all []scoredID
	for _, sh := range shards {
		results, err := sh.idx.Search(context.Background(), q.Vec, topK)
		if err != nil {
			return nil, err
		}
		for _, r := range results {
			id := extIDToInt64(r.ID)
			if id == q.ID {
				continue
			}
			all = append(all, scoredID{id: id, score: r.Score})
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].score == all[j].score {
			return all[i].id < all[j].id
		}
		return all[i].score < all[j].score
	})
	out := make([]int64, 0, topK)
	seen := make(map[int64]struct{})
	for _, s := range all {
		if _, ok := seen[s.id]; ok {
			continue
		}
		seen[s.id] = struct{}{}
		out = append(out, s.id)
		if len(out) >= topK {
			break
		}
	}
	return out, nil
}

func hybridSearch(shards []*shardData, arena *arenaReader, q queryItem, vecs map[int64][]float32) (ids []int64, bm25Ms, rerankMs float64, nCandidates int, err error) {
	t0 := time.Now()
	candSet := make(map[int64]struct{})
	for _, sh := range shards {
		cands, err := bm25Search(sh.db, q.Title, q.ID, bm25Limit)
		if err != nil {
			return nil, 0, 0, 0, err
		}
		for _, id := range cands {
			candSet[id] = struct{}{}
		}
	}
	bm25Ms = float64(time.Since(t0).Microseconds()) / 1000.0

	t1 := time.Now()
	var scores []scoredID
	for id := range candSet {
		vec, ok := vecs[id]
		if !ok {
			v, err := arena.ReadVec(int(id))
			if errors.Is(err, errIDAbsent) {
				continue // candidat lexical sans vecteur : hors espace dense
			}
			if err != nil {
				return nil, bm25Ms, 0, 0, err
			}
			vec = v
		}
		scores = append(scores, scoredID{id: id, score: l2Squared(q.Vec, vec)})
	}
	sort.Slice(scores, func(i, j int) bool {
		if scores[i].score == scores[j].score {
			return scores[i].id < scores[j].id
		}
		return scores[i].score < scores[j].score
	})
	k := topK
	if k > len(scores) {
		k = len(scores)
	}
	ids = make([]int64, k)
	for i := 0; i < k; i++ {
		ids[i] = scores[i].id
	}
	rerankMs = float64(time.Since(t1).Microseconds()) / 1000.0
	return ids, bm25Ms, rerankMs, len(candSet), nil
}

func recallAtK(got []int64, want []int64) float64 {
	if len(want) == 0 {
		return 0
	}
	wantSet := make(map[int64]struct{}, len(want))
	for _, id := range want {
		wantSet[id] = struct{}{}
	}
	hits := 0
	for _, id := range got {
		if _, ok := wantSet[id]; ok {
			hits++
		}
	}
	return float64(hits) / float64(len(want))
}

type benchResults struct {
	hybridRecall  float64
	denseRecall   float64
	hybridP50     float64
	hybridP95     float64
	denseP50      float64
	denseP95      float64
	tableau       string
	decomposition string
}

func runBenchmark(shards []*shardData, queries []queryItem, gt [][]int64, unionTotal int) (benchResults, string, error) {
	arena, err := openArena(arenaPath)
	if err != nil {
		return benchResults{}, "", err
	}
	defer arena.Close()

	unionVecs, err := loadUnionVectors(arena, flattenItems(shards))
	if err != nil {
		return benchResults{}, "", err
	}

	var (
		hybridRecalls   []float64
		denseRecalls    []float64
		hybridLatencies []float64
		denseLatencies  []float64
		bm25Times       []float64
		rerankTimes     []float64
		scopeRatios     []float64
	)

	var b strings.Builder
	b.WriteString("### Sortie benchmark (200 requêtes)\n\n")

	for qi, q := range queries {
		// Dense
		t0 := time.Now()
		denseIDs, err := denseSearch(shards, q)
		if err != nil {
			return benchResults{}, "", fmt.Errorf("dense q%d: %w", qi, err)
		}
		denseMs := float64(time.Since(t0).Microseconds()) / 1000.0
		denseLatencies = append(denseLatencies, denseMs)
		denseRecalls = append(denseRecalls, recallAtK(denseIDs, gt[qi]))

		// Hybrid
		t1 := time.Now()
		hybridIDs, bm25Ms, rerankMs, nCand, err := hybridSearch(shards, arena, q, unionVecs)
		if err != nil {
			return benchResults{}, "", fmt.Errorf("hybrid q%d: %w", qi, err)
		}
		hybridMs := float64(time.Since(t1).Microseconds()) / 1000.0
		hybridLatencies = append(hybridLatencies, hybridMs)
		hybridRecalls = append(hybridRecalls, recallAtK(hybridIDs, gt[qi]))
		bm25Times = append(bm25Times, bm25Ms)
		rerankTimes = append(rerankTimes, rerankMs)
		scopeRatios = append(scopeRatios, float64(nCand)/float64(unionTotal))
	}

	hybridRecallMean := mean(hybridRecalls)
	denseRecallMean := mean(denseRecalls)
	sort.Float64s(hybridLatencies)
	sort.Float64s(denseLatencies)

	res := benchResults{
		hybridRecall: hybridRecallMean,
		denseRecall:  denseRecallMean,
		hybridP50:    percentile(hybridLatencies, 0.50),
		hybridP95:    percentile(hybridLatencies, 0.95),
		denseP50:     percentile(denseLatencies, 0.50),
		denseP95:     percentile(denseLatencies, 0.95),
	}

	tableau := fmt.Sprintf(
		"| méthode | recall@10 moyen | p50 (ms) | p95 (ms) |\n|---------|-----------------|----------|----------|\n| hybride | %.4f | %.3f | %.3f |\n| dense   | %.4f | %.3f | %.3f |",
		hybridRecallMean, res.hybridP50, res.hybridP95,
		denseRecallMean, res.denseP50, res.denseP95,
	)
	res.tableau = tableau

	decomp := fmt.Sprintf(
		"BM25 moyen %.3f ms | rerank moyen %.3f ms | scope réduction moyen %.4f (candidats/union)",
		mean(bm25Times), mean(rerankTimes), mean(scopeRatios),
	)
	res.decomposition = decomp

	b.WriteString(tableau + "\n\n")
	b.WriteString("**Décomposition hybride** : " + decomp + "\n\n")
	seuil := hybridRecallMean >= denseRecallMean-0.005 && res.hybridP95 <= 1.2*res.denseP95
	b.WriteString(fmt.Sprintf("**Seuil** : recall hybride ≥ dense−0,005 → %.4f ≥ %.4f : %v ; p95 hybride ≤ 1,2× dense → %.3f ≤ %.3f : %v\n",
		hybridRecallMean, denseRecallMean-0.005, hybridRecallMean >= denseRecallMean-0.005,
		res.hybridP95, 1.2*res.denseP95, res.hybridP95 <= 1.2*res.denseP95,
	))
	b.WriteString(fmt.Sprintf("**Verdict seuil** : %v\n", seuil))

	return res, b.String(), nil
}

func flattenItems(shards []*shardData) []itemRow {
	var all []itemRow
	for _, sh := range shards {
		all = append(all, sh.items...)
	}
	return all
}

func shardVolumeList(shards []*shardData) []map[string]any {
	out := make([]map[string]any, len(shards))
	for i, sh := range shards {
		out[i] = map[string]any{
			"shard":  sh.spec.label,
			"start":  sh.spec.start,
			"end":    sh.spec.end,
			"volume": len(sh.items),
		}
	}
	return out
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func appendRapport(section string) {
	path := "/tmp/claude-1000/-devhoros/3d1d35ad-01bd-4525-8930-eeb93202b588/scratchpad/chantier-p1/rapport.md"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rapport append: %v\n", err)
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "\n%s\n", section)
}
