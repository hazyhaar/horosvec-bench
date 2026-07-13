package main

import (
	"context"
	"log/slog"
	"sort"
	"sync/atomic"

	"github.com/hazyhaar/horosvec"
)

// labeledSearcher associe un label journalisé à un sous-searcher horosvec.
type labeledSearcher struct {
	label string
	s     searcher
}

// federatedSearcher interroge plusieurs index, fusionne par d² croissante et dédoublonne par ext_id.
type federatedSearcher struct {
	members []labeledSearcher
	log     *slog.Logger

	lastIndices atomic.Pointer[[]string]
	lastSkipped atomic.Pointer[[]string]
}

func newFederatedSearcher(log *slog.Logger, members []labeledSearcher) *federatedSearcher {
	if log == nil {
		log = slog.Default()
	}
	return &federatedSearcher{members: members, log: log}
}

// LastIndices restitue les labels interrogés avec succès lors du dernier Search.
func (f *federatedSearcher) LastIndices() []string {
	if p := f.lastIndices.Load(); p != nil {
		return *p
	}
	return nil
}

// LastSkipped restitue les labels sautés (erreur) lors du dernier Search.
func (f *federatedSearcher) LastSkipped() []string {
	if p := f.lastSkipped.Load(); p != nil {
		return *p
	}
	return nil
}

// Search interroge chaque sous-searcher pour topK, fusionne et déduplique par ext_id (plus petit d²).
func (f *federatedSearcher) Search(ctx context.Context, query []float32, topK int) ([]horosvec.Result, error) {
	if topK <= 0 {
		topK = 1
	}
	var merged []horosvec.Result
	indices := make([]string, 0, len(f.members))
	skipped := make([]string, 0)
	for _, m := range f.members {
		res, err := m.s.Search(ctx, query, topK)
		if err != nil {
			skipped = append(skipped, m.label)
			f.log.Warn("federated search: sous-index sauté", "label", m.label, "err", err.Error())
			continue
		}
		indices = append(indices, m.label)
		merged = append(merged, res...)
	}
	f.lastIndices.Store(&indices)
	f.lastSkipped.Store(&skipped)
	return mergeFederatedResults(merged, topK), nil
}

// mergeFederatedResults trie par d² croissant, dédoublonne par ext_id (garde le plus petit d²), tronque à topK.
func mergeFederatedResults(all []horosvec.Result, topK int) []horosvec.Result {
	if len(all) == 0 {
		return nil
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score < all[j].Score
		}
		return string(all[i].ID) < string(all[j].ID)
	})
	seen := make(map[string]struct{}, len(all))
	out := make([]horosvec.Result, 0, topK)
	for _, r := range all {
		id := string(r.ID)
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, r)
		if len(out) >= topK {
			break
		}
	}
	return out
}
