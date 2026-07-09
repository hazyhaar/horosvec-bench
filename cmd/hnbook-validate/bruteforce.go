package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"

	"github.com/hazyhaar/horosvec"
)

// bruteForceTopK établit la vérité exacte en UNE SEULE PASSE séquentielle sur l'arène fp16 :
// chaque worker balaie un bloc contigu de rangs, décode chaque vecteur (fp16→fp32 via
// ArenaReader.VecInto, dans un tampon réutilisé — jamais l'arène entière sur le tas Go), et le
// confronte à TOUTES les requêtes simultanément, maintenant un top-K par requête. Les top-K
// partiels des workers sont ensuite fusionnés. Le métrique est la distance L2 au carré (plus
// petit = plus proche), EXACTEMENT le métrique classé par le rerank du moteur horosvec : la
// force brute mesure donc la vérité de ce que Search classe, sur les mêmes octets décodés à
// l'identique. Pour des vecteurs unitaires, ce classement coïncide avec celui du produit
// scalaire décroissant.
//
// Retourne, par requête, les rangs (node_id) du top-K exact triés du plus proche au plus
// éloigné, ainsi que le nombre de vecteurs de l'arène.
func bruteForceTopK(ctx context.Context, arenaPath string, queries [][]float32, topK, workers int, report *progress) ([][]int64, int64, error) {
	ar, err := horosvec.OpenArenaReader(arenaPath)
	if err != nil {
		return nil, 0, fmt.Errorf("open arena reader: %w", err)
	}
	// ArenaReader est immuable après ouverture ; VecInto est sûr en accès concurrent. Aucune
	// fermeture publique n'est exposée (la cartographie est libérée à la fin du process) —
	// l'arène reste strictement en lecture seule.
	dim := ar.Dim()
	count := ar.Count()
	if dim <= 0 || count <= 0 {
		return nil, 0, fmt.Errorf("arène vide ou invalide (dim=%d count=%d)", dim, count)
	}
	for i, q := range queries {
		if len(q) != dim {
			return nil, 0, fmt.Errorf("requête %d : dimension %d != dimension arène %d", i, len(q), dim)
		}
	}

	nq := len(queries)
	if int64(workers) > count {
		workers = int(count)
	}
	if workers < 1 {
		workers = 1
	}

	// Découpage en blocs contigus de rangs, un par worker.
	partials := make([][]*topHeap, workers)
	var wg sync.WaitGroup
	var cancelled bool
	var cmu sync.Mutex
	chunk := (count + int64(workers) - 1) / int64(workers)
	for w := 0; w < workers; w++ {
		lo := int64(w) * chunk
		hi := lo + chunk
		if hi > count {
			hi = count
		}
		if lo >= hi {
			partials[w] = newTopKSet(nq, topK)
			continue
		}
		wg.Add(1)
		go func(w int, lo, hi int64) {
			defer wg.Done()
			set := newTopKSet(nq, topK)
			buf := make([]float32, dim)
			for r := lo; r < hi; r++ {
				if r%(1<<20) == 0 {
					if ctx.Err() != nil {
						cmu.Lock()
						cancelled = true
						cmu.Unlock()
						break
					}
				}
				if !ar.VecInto(r, buf) {
					continue
				}
				for qi := 0; qi < nq; qi++ {
					d := l2sq(queries[qi], buf)
					set[qi].offer(scoredID{score: d, rank: r})
				}
			}
			partials[w] = set
		}(w, lo, hi)
	}
	wg.Wait()
	if cancelled {
		return nil, 0, ctx.Err()
	}
	report.step("force brute passe terminée (rangs=%d)", count)

	// Fusion des top-K partiels par requête.
	final := make([]*topHeap, nq)
	for qi := 0; qi < nq; qi++ {
		final[qi] = newTopK(topK)
	}
	for w := 0; w < workers; w++ {
		if partials[w] == nil {
			continue
		}
		for qi := 0; qi < nq; qi++ {
			for _, it := range partials[w][qi].items {
				final[qi].offer(it)
			}
		}
	}

	out := make([][]int64, nq)
	for qi := 0; qi < nq; qi++ {
		out[qi] = final[qi].sortedRanks()
	}
	return out, count, nil
}

// l2sq est la distance L2 au carré entre deux vecteurs de même longueur (métrique classé par
// le rerank du moteur : plus petit = plus proche).
func l2sq(a, b []float32) float64 {
	var s float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		s += d * d
	}
	return s
}

// scoredID est un candidat du top-K : un score (distance) et son rang (node_id).
type scoredID struct {
	score float64
	rank  int64
}

// worse retourne vrai si a doit être évincé avant b (a est « pire » : score plus grand, et à
// score égal rang plus grand). L'ordre total déterministe sur (score, rank) rend le top-K
// reproductible même sous égalités de score au bord du seuil.
func worse(a, b scoredID) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	return a.rank > b.rank
}

// topHeap est un tas-max borné à k éléments (racine = le pire candidat conservé), qui retient
// les k candidats les plus proches vus au fil du flux.
type topHeap struct {
	k     int
	items []scoredID
}

func newTopK(k int) *topHeap {
	return &topHeap{k: k, items: make([]scoredID, 0, k)}
}

func newTopKSet(nq, k int) []*topHeap {
	set := make([]*topHeap, nq)
	for i := range set {
		set[i] = newTopK(k)
	}
	return set
}

// offer insère un candidat s'il figure parmi les k plus proches, en évinçant la racine (le
// pire conservé) le cas échéant.
func (h *topHeap) offer(c scoredID) {
	if len(h.items) < h.k {
		h.items = append(h.items, c)
		h.up(len(h.items) - 1)
		return
	}
	// items[0] est le pire conservé. Le candidat n'entre que s'il est meilleur (moins pire).
	if worse(c, h.items[0]) || c == h.items[0] {
		return
	}
	h.items[0] = c
	h.down(0)
}

func (h *topHeap) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if worse(h.items[i], h.items[parent]) {
			h.items[i], h.items[parent] = h.items[parent], h.items[i]
			i = parent
		} else {
			break
		}
	}
}

func (h *topHeap) down(i int) {
	n := len(h.items)
	for {
		l, r := 2*i+1, 2*i+2
		worst := i
		if l < n && worse(h.items[l], h.items[worst]) {
			worst = l
		}
		if r < n && worse(h.items[r], h.items[worst]) {
			worst = r
		}
		if worst == i {
			break
		}
		h.items[i], h.items[worst] = h.items[worst], h.items[i]
		i = worst
	}
}

// sortedRanks retourne les rangs du top-K triés du plus proche au plus éloigné.
func (h *topHeap) sortedRanks() []int64 {
	cp := make([]scoredID, len(h.items))
	copy(cp, h.items)
	sort.Slice(cp, func(i, j int) bool { return worse(cp[j], cp[i]) })
	out := make([]int64, len(cp))
	for i, it := range cp {
		out[i] = it.rank
	}
	return out
}

// idTable est une vue sur le fichier d'ids (uint64 LE, rang = node_id) qui décode l'ext_id
// À LA DEMANDE (décimal ASCII, identique à horosvec.readArenaIDs). Sur le run 26,7 M, seuls
// les nq×topK rangs retenus par la force brute sont décodés : la table ne matérialise jamais
// les 26,7 M chaînes sur le tas (elle ne garde que les octets bruts, ~8 octets par nœud).
type idTable struct {
	raw []byte
	n   int64
}

// readIDs lit et valide le fichier d'ids (taille multiple de 8, compte == compte d'arène).
func readIDs(path string, arenaCount int64) (*idTable, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data)%8 != 0 {
		return nil, fmt.Errorf("fichier d'ids %s : taille %d non multiple de 8", path, len(data))
	}
	n := int64(len(data) / 8)
	if n != arenaCount {
		return nil, fmt.Errorf("fichier d'ids : %d ids != %d vecteurs d'arène", n, arenaCount)
	}
	return &idTable{raw: data, n: n}, nil
}

// at décode l'ext_id du rang donné en décimal ASCII.
func (t *idTable) at(rank int64) string {
	id := binary.LittleEndian.Uint64(t.raw[rank*8:])
	return strconv.FormatUint(id, 10)
}

// sortFloat64 trie un échantillon de latences en place (croissant).
func sortFloat64(xs []float64) {
	sort.Float64s(xs)
}
