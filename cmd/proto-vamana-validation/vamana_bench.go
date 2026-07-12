// Port fidèle de la construction Vamana horosvec (vamana.go) pour le banc — hors moteur.
package main

import (
	"context"
	"math"
	"math/rand/v2"
)

const (
	slotSelf  = 0
	slotBest  = 1
	slotProbe = 2
)

type searchCandidate struct {
	nodeID int64
	dist   float64
}

type benchNode struct {
	vec    []float32
	rot    []float32 // rotated (codeDim), pour distances RaBitQ à la construction
	code   []byte
	sqNorm float64
	l1Norm float64
}

// distMode sélectionne la métrique utilisée pendant la construction du graphe.
type distMode int

const (
	distFP32 distMode = iota
	distRaBitQ
)

type benchVecSource struct {
	nodes    []*benchNode
	mode     distMode
	centroid []float32 // espace roté, pour distRaBitQ
}

func (vs *benchVecSource) vec(id int64, slot int) []float32 {
	if id < 0 || int(id) >= len(vs.nodes) {
		return nil
	}
	switch vs.mode {
	case distFP32:
		return vs.nodes[id].vec
	case distRaBitQ:
		switch slot {
		case slotSelf, slotBest:
			return vs.nodes[id].rot
		default:
			return vs.nodes[id].rot
		}
	default:
		return vs.nodes[id].vec
	}
}

func (vs *benchVecSource) clone() *benchVecSource {
	return vs // séquentiel uniquement
}

func (vs *benchVecSource) distance(queryID, storedID int64) float64 {
	if queryID < 0 || storedID < 0 || int(queryID) >= len(vs.nodes) || int(storedID) >= len(vs.nodes) {
		return math.MaxFloat64
	}
	switch vs.mode {
	case distFP32:
		return l2sq(vs.nodes[queryID].vec, vs.nodes[storedID].vec)
	default:
		n := vs.nodes[storedID]
		return rabitqDistAsym(vs.nodes[queryID].rot, vs.centroid, n.code, n.sqNorm, n.l1Norm)
	}
}

type flatNeighborStore struct {
	n, stride int
	nbrs      []int32
	deg       []int32
}

func newFlatNeighborStore(n, maxDegree int) *flatNeighborStore {
	stride := 2*maxDegree + 1
	return &flatNeighborStore{
		n:      n,
		stride: stride,
		nbrs:   make([]int32, n*stride),
		deg:    make([]int32, n),
	}
}

func (s *flatNeighborStore) loadInto(id int64, dst []int64) []int64 {
	base := int(id) * s.stride
	d := int(s.deg[id])
	for i := 0; i < d; i++ {
		dst = append(dst, int64(s.nbrs[base+i]))
	}
	return dst
}

func (s *flatNeighborStore) set(id int64, nbrs []int64) {
	base := int(id) * s.stride
	d := len(nbrs)
	if d > s.stride {
		d = s.stride
	}
	for i := 0; i < d; i++ {
		s.nbrs[base+i] = int32(nbrs[i])
	}
	s.deg[id] = int32(d)
}

func initRandomNeighborsStore(rng *rand.Rand, store *flatNeighborStore, n, maxDegree int) {
	nNeighbors := maxDegree
	if nNeighbors > n-1 {
		nNeighbors = n - 1
	}
	if nNeighbors <= 0 {
		return
	}
	pool := make([]int64, n)
	for i := range n {
		pool[i] = int64(i)
	}
	pickBuf := make([]int, nNeighbors)
	drawn := make([]int64, nNeighbors)
	for i := range n {
		if store.deg[int64(i)] > 0 {
			continue
		}
		myIdx := i
		pool[myIdx], pool[n-1] = pool[n-1], pool[myIdx]
		for j := range nNeighbors {
			ri := rng.IntN(n - 1 - j)
			drawn[j] = pool[ri]
			pickBuf[j] = ri
			pool[ri], pool[n-2-j] = pool[n-2-j], pool[ri]
		}
		store.set(int64(i), drawn)
		for j := nNeighbors - 1; j >= 0; j-- {
			ri := pickBuf[j]
			pool[ri], pool[n-2-j] = pool[n-2-j], pool[ri]
		}
		pool[myIdx], pool[n-1] = pool[n-1], pool[myIdx]
	}
}

func buildVamanaGraph(
	ctx context.Context,
	nodes []*benchNode,
	centroid []float32,
	mode distMode,
	medoid int64,
	maxDegree, beamWidth int,
	alpha float64,
	passes int,
) (*flatNeighborStore, error) {
	n := len(nodes)
	if n == 0 {
		return newFlatNeighborStore(0, maxDegree), nil
	}
	vs := &benchVecSource{nodes: nodes, mode: mode, centroid: centroid}
	store := newFlatNeighborStore(n, maxDegree)
	rng := rand.New(rand.NewPCG(42, 0))
	initRandomNeighborsStore(rng, store, n, maxDegree)

	nbrBuf := make([]int64, 0, store.stride)
	rmwBuf := make([]int64, 0, store.stride)
	finalBuf := make([]int64, 0, store.stride)

	getNeighbors := func(id int64) []int64 {
		if id < 0 || int(id) >= n {
			return nil
		}
		return store.loadInto(id, nbrBuf[:0])
	}

	order := make([]int, n)
	for i := range n {
		order[i] = i
	}

	for pass := range passes {
		_ = pass
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for i := n - 1; i > 0; i-- {
			j := rng.IntN(i + 1)
			order[i], order[j] = order[j], order[i]
		}
		for _, oi := range order {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			nid := int64(oi)
			candidates, _ := greedySearchBench(nid, medoid, beamWidth, vs, getNeighbors)
			for _, nbr := range getNeighbors(nid) {
				d := vs.distance(nid, nbr)
				candidates = append(candidates, searchCandidate{nodeID: nbr, dist: d})
			}
			newNeighbors := robustPruneBench(nid, candidates, alpha, maxDegree, vs)
			store.set(nid, newNeighbors)
			for _, nbr := range newNeighbors {
				if nbr < 0 || int(nbr) >= n {
					continue
				}
				cur := store.loadInto(nbr, rmwBuf[:0])
				found := false
				for _, nn := range cur {
					if nn == nid {
						found = true
						break
					}
				}
				if found {
					continue
				}
				updated := append(cur, nid)
				if len(updated) > 2*maxDegree {
					cands := make([]searchCandidate, len(updated))
					for ci, nn := range updated {
						cands[ci] = searchCandidate{nodeID: nn, dist: vs.distance(nbr, nn)}
					}
					updated = robustPruneBench(nbr, cands, alpha, maxDegree, vs)
				}
				store.set(nbr, updated)
			}
		}
	}

	for i := 0; i < n; i++ {
		if store.deg[int64(i)] <= int32(maxDegree) {
			continue
		}
		cur := store.loadInto(int64(i), finalBuf[:0])
		cands := make([]searchCandidate, len(cur))
		for ci, nn := range cur {
			cands[ci] = searchCandidate{nodeID: nn, dist: vs.distance(int64(i), nn)}
		}
		store.set(int64(i), robustPruneBench(int64(i), cands, alpha, maxDegree, vs))
	}
	return store, nil
}

func greedySearchBench(
	queryID int64,
	start int64,
	beamWidth int,
	vs *benchVecSource,
	getNeighbors func(int64) []int64,
) ([]searchCandidate, map[int64]bool) {
	visited := make(map[int64]bool)
	startDist := vs.distance(queryID, start)
	visited[start] = true

	h := candidateHeap{{nodeID: start, dist: startDist}}
	heapInit(&h)

	best := []searchCandidate{{nodeID: start, dist: startDist}}
	worstBest := startDist

	for h.Len() > 0 {
		cur := heapPop(&h)
		if len(best) >= beamWidth && cur.dist > worstBest {
			break
		}
		for _, nbr := range getNeighbors(cur.nodeID) {
			if visited[nbr] {
				continue
			}
			visited[nbr] = true
			d := vs.distance(queryID, nbr)
			if len(best) < beamWidth || d < worstBest {
				heapPush(&h, searchCandidate{nodeID: nbr, dist: d})
				best = insertSortedCand(best, searchCandidate{nodeID: nbr, dist: d})
				if len(best) > beamWidth {
					best = best[:beamWidth]
				}
				worstBest = best[len(best)-1].dist
			}
		}
	}
	return best, visited
}

func robustPruneBench(
	nodeID int64,
	candidates []searchCandidate,
	alpha float64,
	maxDegree int,
	vs *benchVecSource,
) []int64 {
	seen := map[int64]bool{nodeID: true}
	filtered := make([]searchCandidate, 0, len(candidates))
	for _, c := range candidates {
		if seen[c.nodeID] {
			continue
		}
		seen[c.nodeID] = true
		filtered = append(filtered, c)
	}
	sortCandidates(filtered)
	result := make([]int64, 0, maxDegree)
	for len(filtered) > 0 && len(result) < maxDegree {
		best := filtered[0]
		filtered = filtered[1:]
		result = append(result, best.nodeID)
		kept := filtered[:0]
		for _, c := range filtered {
			distToBest := vs.distance(best.nodeID, c.nodeID)
			if alpha*distToBest > c.dist {
				kept = append(kept, c)
			}
		}
		filtered = kept
	}
	return result
}

func sortCandidates(candidates []searchCandidate) {
	for i := 1; i < len(candidates); i++ {
		key := candidates[i]
		j := i - 1
		for j >= 0 && candidates[j].dist > key.dist {
			candidates[j+1] = candidates[j]
			j--
		}
		candidates[j+1] = key
	}
}

func insertSortedCand(sorted []searchCandidate, c searchCandidate) []searchCandidate {
	lo, hi := 0, len(sorted)
	for lo < hi {
		mid := (lo + hi) / 2
		if sorted[mid].dist < c.dist {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	sorted = append(sorted, searchCandidate{})
	copy(sorted[lo+1:], sorted[lo:])
	sorted[lo] = c
	return sorted
}

// min-heap pour greedy search
type candidateHeap []searchCandidate

func (h candidateHeap) Len() int           { return len(h) }
func (h candidateHeap) Less(i, j int) bool { return h[i].dist < h[j].dist }
func (h candidateHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

func heapInit(h *candidateHeap) {
	for i := len(*h)/2 - 1; i >= 0; i-- {
		heapSiftDown(h, i)
	}
}

func heapPush(h *candidateHeap, x searchCandidate) {
	*h = append(*h, x)
	heapSiftUp(h, len(*h)-1)
}

func heapPop(h *candidateHeap) searchCandidate {
	old := *h
	n := len(old)
	x := old[0]
	old[0] = old[n-1]
	*h = old[:n-1]
	if len(*h) > 0 {
		heapSiftDown(h, 0)
	}
	return x
}

func heapSiftUp(h *candidateHeap, i int) {
	for i > 0 {
		p := (i - 1) / 2
		if (*h)[p].dist <= (*h)[i].dist {
			break
		}
		(*h)[p], (*h)[i] = (*h)[i], (*h)[p]
		i = p
	}
}

func heapSiftDown(h *candidateHeap, i int) {
	n := len(*h)
	for {
		l, r, smallest := 2*i+1, 2*i+2, i
		if l < n && (*h)[l].dist < (*h)[smallest].dist {
			smallest = l
		}
		if r < n && (*h)[r].dist < (*h)[smallest].dist {
			smallest = r
		}
		if smallest == i {
			break
		}
		(*h)[i], (*h)[smallest] = (*h)[smallest], (*h)[i]
		i = smallest
	}
}

func findMedoidFP32(nodes []*benchNode) int64 {
	if len(nodes) == 0 {
		return 0
	}
	dim := len(nodes[0].vec)
	centroid := make([]float64, dim)
	for _, n := range nodes {
		for j, v := range n.vec {
			centroid[j] += float64(v)
		}
	}
	invN := 1.0 / float64(len(nodes))
	cf := make([]float32, dim)
	for j := range dim {
		cf[j] = float32(centroid[j] * invN)
	}
	bestID := int64(0)
	bestDist := math.MaxFloat64
	for i, n := range nodes {
		d := l2sq(cf, n.vec)
		if d < bestDist {
			bestDist = d
			bestID = int64(i)
		}
	}
	return bestID
}

// searchGraph : marche greedy 1-bit + rerank exact fp32 top-M.
func searchGraph(
	store *flatNeighborStore,
	nodes []*benchNode,
	centroid []float32,
	medoid int64,
	queryRot []float32,
	queryRaw []float32,
	efSearch, rerankM, topK int,
) []int {
	n := len(nodes)
	nbrBuf := make([]int64, 0, store.stride)
	getNeighbors := func(id int64) []int64 {
		return store.loadInto(id, nbrBuf[:0])
	}

	// Pré-calcul requête centrée (espace roté)
	var querySq float64
	qc := make([]float64, len(queryRot))
	for i := range queryRot {
		c := float64(queryRot[i]) - float64(centroid[i])
		qc[i] = c
		querySq += c * c
	}

	visited := make(map[int64]bool)
	startDist := rabitqDistPrecomp(qc, querySq, nodes[medoid].code, nodes[medoid].sqNorm, nodes[medoid].l1Norm)
	visited[medoid] = true

	h := candidateHeap{{nodeID: medoid, dist: startDist}}
	heapInit(&h)
	best := []searchCandidate{{nodeID: medoid, dist: startDist}}
	worstBest := startDist

	for h.Len() > 0 {
		cur := heapPop(&h)
		if len(best) >= efSearch && cur.dist > worstBest {
			break
		}
		for _, nbr := range getNeighbors(cur.nodeID) {
			if visited[nbr] || nbr < 0 || int(nbr) >= n {
				continue
			}
			visited[nbr] = true
			nd := nodes[nbr]
			d := rabitqDistPrecomp(qc, querySq, nd.code, nd.sqNorm, nd.l1Norm)
			if len(best) < efSearch || d < worstBest {
				heapPush(&h, searchCandidate{nodeID: nbr, dist: d})
				best = insertSortedCand(best, searchCandidate{nodeID: nbr, dist: d})
				if len(best) > efSearch {
					best = best[:efSearch]
				}
				worstBest = best[len(best)-1].dist
			}
		}
	}

	if len(best) > rerankM {
		best = best[:rerankM]
	}
	reranked := make([]scoredCand, len(best))
	for i, c := range best {
		reranked[i] = scoredCand{id: int(c.nodeID), d: l2sq(queryRaw, nodes[c.nodeID].vec)}
	}
	sortScored(reranked)
	if len(reranked) > topK {
		reranked = reranked[:topK]
	}
	out := make([]int, len(reranked))
	for i, s := range reranked {
		out[i] = s.id
	}
	return out
}

type scoredCand struct {
	id int
	d  float64
}

func sortScored(s []scoredCand) {
	for i := 1; i < len(s); i++ {
		key := s[i]
		j := i - 1
		for j >= 0 && s[j].d > key.d {
			s[j+1] = s[j]
			j--
		}
		s[j+1] = key
	}
}
