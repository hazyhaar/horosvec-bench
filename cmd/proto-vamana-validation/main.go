// Command proto-vamana-validation is a THROWAWAY decidable oracle for the Vamana graph
// quality and db-blob incremental insert path on a REAL dim-512 corpus (HVARENA1).
//
// (a) Builds two Vamana graphs off-engine (fp32 vs RaBitQ 1-bit construction distances),
//
//	measures recall@10 (greedy 1-bit + fp32 rerank top-128) vs brute-force GT.
//
// (b) Exercises horosvec.New/Build/Insert (db-blob) with incremental inserts vs rebuild.
// (c) Reports runtime.MemStats at build and after inserts.
// (d) Binary verdict on db-blob incremental qualification.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/hazyhaar/horosvec"
	_ "modernc.org/sqlite"
)

const (
	arenaPath   = "/inference/hnbook/bench_final/prefix1m.arena"
	nBase       = 11600
	nQuery      = 200
	k           = 10
	efSearch    = 128
	rerankM     = 128
	maxDegree   = 64
	beamBuild   = 128
	buildPasses = 2
	alpha       = 1.2
)

type sliceIter struct {
	vecs [][]float32
	ids  [][]byte
	i    int
}

func (s *sliceIter) Next() (id []byte, vec []float32, ok bool) {
	if s.i >= len(s.vecs) {
		return nil, nil, false
	}
	id = s.ids[s.i]
	vec = s.vecs[s.i]
	s.i++
	return id, vec, true
}

func (s *sliceIter) Reset() error { s.i = 0; return nil }

func memMB() (heapAlloc, sys uint64) {
	var ms runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc, ms.Sys
}

func encodeBenchNodes(base [][]float32, rot *rotator, centroid []float32) []*benchNode {
	cd := rot.codeDim
	scratch := make([]float64, cd)
	nodes := make([]*benchNode, len(base))
	for i, v := range base {
		rotated := make([]float32, cd)
		rot.rotate(v, rotated, scratch)
		code, sq, l1 := encode1bit(rotated, centroid)
		vecCopy := make([]float32, len(v))
		copy(vecCopy, v)
		rotCopy := make([]float32, cd)
		copy(rotCopy, rotated)
		nodes[i] = &benchNode{
			vec:    vecCopy,
			rot:    rotCopy,
			code:   code,
			sqNorm: sq,
			l1Norm: l1,
		}
	}
	return nodes
}

func computeRotCentroid(base [][]float32, rot *rotator) []float32 {
	cd := rot.codeDim
	scratch := make([]float64, cd)
	acc := make([]float64, cd)
	x := make([]float32, cd)
	for _, v := range base {
		rot.rotate(v, x, scratch)
		for j := range cd {
			acc[j] += float64(x[j])
		}
	}
	inv := 1.0 / float64(len(base))
	centroid := make([]float32, cd)
	for j := range cd {
		centroid[j] = float32(acc[j] * inv)
	}
	return centroid
}

func runPartA(base, queries [][]float32, gt [][]int) (table string, delta float64) {
	rot := newRotator(len(base[0]))
	centroid := computeRotCentroid(base, rot)
	nodes := encodeBenchNodes(base, rot, centroid)
	medoid := findMedoidFP32(nodes)
	ctx := context.Background()

	t0 := time.Now()
	storeFP32, err := buildVamanaGraph(ctx, nodes, centroid, distFP32, medoid, maxDegree, beamBuild, alpha, buildPasses)
	must(err)
	fmt.Fprintf(os.Stderr, "graph fp32 built in %s\n", time.Since(t0))

	t0 = time.Now()
	storeRQ, err := buildVamanaGraph(ctx, nodes, centroid, distRaBitQ, medoid, maxDegree, beamBuild, alpha, buildPasses)
	must(err)
	fmt.Fprintf(os.Stderr, "graph rabitq built in %s\n", time.Since(t0))

	scratch := make([]float64, rot.codeDim)
	qRot := make([][]float32, nQuery)
	for qi, q := range queries {
		x := make([]float32, rot.codeDim)
		rot.rotate(q, x, scratch)
		qRot[qi] = x
	}

	var sumFP32, sumRQ float64
	for qi := 0; qi < nQuery; qi++ {
		gotFP32 := searchGraph(storeFP32, nodes, centroid, medoid, qRot[qi], queries[qi], efSearch, rerankM, k)
		gotRQ := searchGraph(storeRQ, nodes, centroid, medoid, qRot[qi], queries[qi], efSearch, rerankM, k)
		sumFP32 += recallAt(gotFP32, gt[qi])
		sumRQ += recallAt(gotRQ, gt[qi])
	}
	recFP32 := sumFP32 / float64(nQuery)
	recRQ := sumRQ / float64(nQuery)
	delta = recFP32 - recRQ

	table = fmt.Sprintf(
		"%-28s  %10s  %10s\n"+
			"%-28s  %10s  %10s\n"+
			"%-28s  %10.4f  %10.4f\n"+
			"%-28s  %10.4f  %10.4f\n"+
			"%-28s  %10.4f  %10s\n",
		"graphe", "recall@10", "écart vs fp32",
		"----------------------------", "---------", "----------",
		"construction fp32", recFP32, 0.0,
		"construction RaBitQ 1-bit", recRQ, delta,
		"GT brute-force exacte", 1.0, "—",
	)
	return table, delta
}

func engineConfig() horosvec.Config {
	cfg := horosvec.DefaultConfig()
	cfg.BruteForceThreshold = 0
	cfg.BuildWorkers = 1
	cfg.EfSearch = efSearch
	cfg.RerankTopN = rerankM
	return cfg
}

func openTempDB() (*sql.DB, string, func()) {
	tmp, err := os.CreateTemp("", "vamana-val-*.db")
	must(err)
	path := tmp.Name()
	tmp.Close()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)")
	must(err)
	cleanup := func() {
		db.Close()
		os.Remove(path)
	}
	return db, path, cleanup
}

func buildEngine(vecs [][]float32) (*horosvec.Index, func(), uint64, uint64) {
	db, _, cleanup := openTempDB()
	cfg := engineConfig()
	idx, err := horosvec.New(db, cfg)
	must(err)
	ids := make([][]byte, len(vecs))
	for i := range vecs {
		ids[i] = makeExtID(i)
	}
	iter := &sliceIter{vecs: vecs, ids: ids}
	must(idx.Build(context.Background(), iter))
	ha, sy := memMB()
	cl := func() {
		idx.Close()
		cleanup()
	}
	return idx, cl, ha, sy
}

func measureRecall(idx *horosvec.Index, base [][]float32, queries [][]float32) float64 {
	gt := make([][]int, len(queries))
	for qi, q := range queries {
		gt[qi] = topKExact(base, q, k)
	}
	var sum float64
	for qi, q := range queries {
		res, err := idx.Search(context.Background(), q, k)
		must(err)
		got := make([]int, 0, len(res))
		for _, r := range res {
			// ext_id → index
			var id int
			fmt.Sscanf(string(r.ID), "%d", &id)
			got = append(got, id)
		}
		sum += recallAt(got, gt[qi])
	}
	return sum / float64(len(queries))
}

func measureQPS(idx *horosvec.Index, queries [][]float32, warmup, measureDur time.Duration) float64 {
	for i := 0; i < 50; i++ {
		_, _ = idx.Search(context.Background(), queries[i%nQuery], k)
	}
	deadline := time.Now().Add(measureDur)
	var n int
	for time.Now().Before(deadline) {
		_, _ = idx.Search(context.Background(), queries[n%nQuery], k)
		n++
	}
	return float64(n) / measureDur.Seconds()
}

func runPartB(allBase, queries [][]float32) (table string, maxDrop float64, verdict string) {
	buildN := len(allBase) / 2
	remain := len(allBase) - buildN
	tiers := []struct {
		label string
		frac  float64
	}{
		{"0%", 0},
		{"10%", 0.10},
		{"30%", 0.30},
		{"50%", 0.50},
	}

	type row struct {
		label     string
		n         int
		incRecall float64
		rebRecall float64
		incQPS    float64
		rebQPS    float64
		drop      float64
	}
	var rows []row

	ids := make([][]byte, len(allBase))
	for i := range allBase {
		ids[i] = makeExtID(i)
	}

	for _, tier := range tiers {
		totalN := buildN + int(float64(remain)*tier.frac)
		insertN := totalN - buildN

		// --- incremental ---
		db, _, cleanup := openTempDB()
		cfg := engineConfig()
		idx, err := horosvec.New(db, cfg)
		must(err)

		buildVecs := allBase[:buildN]
		buildIDs := ids[:buildN]
		must(idx.Build(context.Background(), &sliceIter{vecs: buildVecs, ids: buildIDs}))

		if insertN > 0 {
			must(idx.Insert(context.Background(), allBase[buildN:totalN], ids[buildN:totalN]))
		}

		curBase := allBase[:totalN]
		incRecall := measureRecall(idx, curBase, queries)
		incQPS := measureQPS(idx, queries, time.Second, 3*time.Second)
		idx.Close()
		cleanup()

		// --- rebuild at same size ---
		rebIdx, rebClose, _, _ := buildEngine(curBase)
		rebRecall := measureRecall(rebIdx, curBase, queries)
		rebQPS := measureQPS(rebIdx, queries, time.Second, 3*time.Second)
		rebClose()

		drop := rebRecall - incRecall
		if drop > maxDrop {
			maxDrop = drop
		}
		rows = append(rows, row{
			label: tier.label, n: totalN,
			incRecall: incRecall, rebRecall: rebRecall,
			incQPS: incQPS, rebQPS: rebQPS,
			drop: drop,
		})
	}

	table = fmt.Sprintf("%-6s  %6s  %10s  %10s  %10s  %10s  %8s\n",
		"tier", "n", "inc_rec@10", "reb_rec@10", "inc_qps", "reb_qps", "Δrec")
	table += fmt.Sprintf("%-6s  %6s  %10s  %10s  %10s  %10s  %8s\n",
		"------", "------", "----------", "----------", "-------", "-------", "------")
	for _, r := range rows {
		alert := ""
		if r.drop > 0.01 {
			alert = " ALERTE"
		}
		table += fmt.Sprintf("%-6s  %6d  %10.4f  %10.4f  %10.1f  %10.1f  %8.4f%s\n",
			r.label, r.n, r.incRecall, r.rebRecall, r.incQPS, r.rebQPS, r.drop, alert)
	}

	if maxDrop <= 0.01 {
		verdict = "OUI"
	} else {
		verdict = "NON"
	}
	return table, maxDrop, verdict
}

// runPartCMonth : mesure (c) seule sur shard-mois — build db-blob à 50 % de
// nMois puis inserts jusqu'à 150 % du build, RSS relevé aux deux points. Mode
// activé par HVP0_ONLY=c-mois (env), nMois surchargé par HVP0_NMOIS.
func runPartCMonth() {
	t0 := time.Now()
	nMois := 350000
	if v := os.Getenv("HVP0_NMOIS"); v != "" {
		fmt.Sscanf(v, "%d", &nMois)
	}
	all, dim := readArena(arenaPath, nMois)
	fmt.Fprintf(os.Stderr, "loaded %d vecs dim=%d in %s\n", len(all), dim, time.Since(t0))
	half := nMois / 2
	totalN := half + half/2
	ids := make([][]byte, totalN)
	for i := range ids {
		ids[i] = makeExtID(i)
	}
	db, _, cleanup := openTempDB()
	cfg := engineConfig()
	idx, err := horosvec.New(db, cfg)
	must(err)
	must(idx.Build(context.Background(), &sliceIter{vecs: all[:half], ids: ids[:half]}))
	heapBuild, sysBuild := memMB()
	fmt.Println("\n=== (c) Pic RSS shard-mois (runtime.MemStats) ===")
	fmt.Printf("build 50%% shard-mois (n=%d): HeapAlloc=%.1f MiB  Sys=%.1f MiB  (build en %s)\n",
		half, float64(heapBuild)/1024/1024, float64(sysBuild)/1024/1024, time.Since(t0))
	tIns := time.Now()
	must(idx.Insert(context.Background(), all[half:totalN], ids[half:totalN]))
	heapIns, sysIns := memMB()
	fmt.Printf("après inserts +50%% (n=%d): HeapAlloc=%.1f MiB  Sys=%.1f MiB  (inserts en %s)\n",
		totalN, float64(heapIns)/1024/1024, float64(sysIns)/1024/1024, time.Since(tIns))
	idx.Close()
	cleanup()
	fmt.Fprintf(os.Stderr, "\ntotal elapsed %s\n", time.Since(t0))
}

func main() {
	if os.Getenv("HVP0_ONLY") == "c-mois" {
		runPartCMonth()
		return
	}
	t0 := time.Now()
	all, dim := readArena(arenaPath, nBase+nQuery)
	fmt.Fprintf(os.Stderr, "loaded %d vecs dim=%d in %s\n", len(all), dim, time.Since(t0))
	base := all[:nBase]
	queries := all[nBase : nBase+nQuery]

	// GT brute-force pour (a)
	fmt.Fprintf(os.Stderr, "computing GT for part (a)...\n")
	gtA := make([][]int, nQuery)
	for qi, q := range queries {
		gtA[qi] = topKExact(base, q, k)
	}

	fmt.Println("\n=== (a) Graphe bâti quantifié vs fp32 ===")
	tableA, deltaA := runPartA(base, queries, gtA)
	fmt.Print(tableA)
	fmt.Printf("écart recall@10 (fp32 − RaBitQ build) = %.4f\n", deltaA)

	// (c) RSS au build initial (50% shard)
	fmt.Fprintf(os.Stderr, "\nmeasuring RSS at build (50%% shard)...\n")
	buildVecs := base[:nBase/2]
	_, closeBuild, heapBuild, sysBuild := buildEngine(buildVecs)
	closeBuild()

	fmt.Println("\n=== (c) Pic RSS (runtime.MemStats) ===")
	fmt.Printf("build 50%% shard (n=%d): HeapAlloc=%.1f MiB  Sys=%.1f MiB\n",
		len(buildVecs), float64(heapBuild)/1024/1024, float64(sysBuild)/1024/1024)

	fmt.Println("\n=== (b) Inserts dynamiques db-blob ===")
	tableB, maxDrop, verdict := runPartB(base, queries)
	fmt.Print(tableB)
	fmt.Printf("chute max recall (rebuild − incrémental) = %.4f\n", maxDrop)

	// RSS après inserts complets (tier 50%)
	fmt.Fprintf(os.Stderr, "\nmeasuring RSS after inserts (tier 50%%)...\n")
	totalN := nBase/2 + int(float64(nBase/2)*0.50)
	db, _, cleanup := openTempDB()
	cfg := engineConfig()
	idx, _ := horosvec.New(db, cfg)
	ids := make([][]byte, totalN)
	for i := range ids {
		ids[i] = makeExtID(i)
	}
	must(idx.Build(context.Background(), &sliceIter{vecs: base[:nBase/2], ids: ids[:nBase/2]}))
	must(idx.Insert(context.Background(), base[nBase/2:totalN], ids[nBase/2:totalN]))
	heapIns, sysIns := memMB()
	idx.Close()
	cleanup()
	fmt.Printf("après inserts tier 50%% (n=%d): HeapAlloc=%.1f MiB  Sys=%.1f MiB\n",
		totalN, float64(heapIns)/1024/1024, float64(sysIns)/1024/1024)
	fmt.Println("borne shard-mois (350k): non mesuré — temps insuffisant pour ce run")

	fmt.Println("\n=== (d) Verdict ===")
	fmt.Printf("db-blob qualifiable d'incrémental : %s\n", verdict)
	fmt.Printf("(motif: chute max recall rebuild−incrémental = %.4f, seuil 0.01)\n", maxDrop)

	fmt.Fprintf(os.Stderr, "\ntotal elapsed %s\n", time.Since(t0))
}
