package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hazyhaar/horosvec"
)

// monthlyShard décrit une paire index SQLite + arène fp16 publiée dans -shards-dir.
type monthlyShard struct {
	label     string
	indexPath string
	arenaPath string
}

// indexOpener ouvre un horosvec.Index et retourne une fermeture ; utilisé pour les tests.
type indexOpener func(indexPath, arenaPath string) (*horosvec.Index, func(), error)

// discoverMonthlyShards balaie dir à la recherche de paires *.db + arène adjacente.
func discoverMonthlyShards(dir string) ([]monthlyShard, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read shards dir: %w", err)
	}
	var out []monthlyShard
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		dbPath := filepath.Join(dir, e.Name())
		base := strings.TrimSuffix(e.Name(), ".db")
		arenaPath := ""
		for _, candidate := range []string{
			dbPath + ".arena",
			filepath.Join(dir, base+".arena"),
		} {
			if st, statErr := os.Stat(candidate); statErr == nil && !st.IsDir() {
				arenaPath = candidate
				break
			}
		}
		if arenaPath == "" {
			continue
		}
		out = append(out, monthlyShard{
			label:     "month:" + base,
			indexPath: dbPath,
			arenaPath: arenaPath,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].label < out[j].label })
	return out, nil
}

// openShardBlob ouvre le shard courant db-blob en lecture seule (snapshot, sans arène).
func openShardBlob(path string) (*horosvec.Index, func(), error) {
	dsn := path + "?mode=ro&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open shard sqlite: %w", err)
	}
	cfg := horosvec.DefaultConfig()
	if cfg.ArenaPath != "" {
		_ = db.Close()
		return nil, nil, fmt.Errorf("refus arène: shard db-blob exige ArenaPath vide")
	}
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("horosvec new (shard db-blob): %w", err)
	}
	return idx, func() { _ = db.Close() }, nil
}

type openedIndex struct {
	label   string
	idx     *horosvec.Index
	closeDB func()
}

// federatedBundle regroupe le searcher publié et les fermetures monolithe/reloadables.
type federatedBundle struct {
	searcher      *federatedSearcher
	monolith      *horosvec.Index
	monolithClose func()
	reloadClosers []func()
	labels        []string
}

func appendOpened(o openedIndex, opened *[]openedIndex, reloadClosers *[]func()) {
	*opened = append(*opened, o)
	if o.closeDB != nil {
		*reloadClosers = append(*reloadClosers, func(oi openedIndex) func() {
			return func() {
				_ = oi.idx.Close()
				oi.closeDB()
			}
		}(o))
	}
}

// buildFederatedIndex ouvre le monolithe (requis) + shard courant + arènes mensuelles optionnels.
func buildFederatedIndex(log *slog.Logger, monolithIndex, monolithArena, shardPath, shardsDir string, open indexOpener) (*federatedBundle, error) {
	if open == nil {
		open = openIndex
	}
	var opened []openedIndex
	var reloadClosers []func()

	mono, closeMono, err := open(monolithIndex, monolithArena)
	if err != nil {
		return nil, fmt.Errorf("monolith: %w", err)
	}
	opened = append(opened, openedIndex{label: "monolith", idx: mono})

	if shardPath != "" {
		sh, closeSh, err := openShardBlob(shardPath)
		if err != nil {
			log.Warn("shard courant illisible, ignoré", "path", shardPath, "err", err.Error())
		} else {
			appendOpened(openedIndex{label: "shard", idx: sh, closeDB: closeSh}, &opened, &reloadClosers)
		}
	}

	if shardsDir != "" {
		monthly, err := discoverMonthlyShards(shardsDir)
		if err != nil {
			log.Warn("balayage shards-dir échoué", "dir", shardsDir, "err", err.Error())
		} else {
			for _, m := range monthly {
				idx, closeDB, err := open(m.indexPath, m.arenaPath)
				if err != nil {
					log.Warn("arène mensuelle illisible, ignorée", "label", m.label, "err", err.Error())
					continue
				}
				appendOpened(openedIndex{label: m.label, idx: idx, closeDB: closeDB}, &opened, &reloadClosers)
			}
		}
	}

	members := make([]labeledSearcher, len(opened))
	labels := make([]string, len(opened))
	for i, o := range opened {
		members[i] = labeledSearcher{label: o.label, s: o.idx}
		labels[i] = o.label
	}
	return &federatedBundle{
		searcher:      newFederatedSearcher(log, members),
		monolith:      mono,
		monolithClose: func() { _ = mono.Close(); closeMono() },
		reloadClosers: reloadClosers,
		labels:        labels,
	}, nil
}

// rebuildReloadable reconstruit shard + arènes mensuelles autour du monolithe déjà ouvert.
func rebuildReloadable(log *slog.Logger, monolith *horosvec.Index, shardPath, shardsDir string) (*federatedSearcher, []func(), []string) {
	var opened []openedIndex
	var reloadClosers []func()

	opened = append(opened, openedIndex{label: "monolith", idx: monolith})

	if shardPath != "" {
		sh, closeSh, err := openShardBlob(shardPath)
		if err != nil {
			log.Warn("reload shard courant illisible, ignoré", "path", shardPath, "err", err.Error())
		} else {
			appendOpened(openedIndex{label: "shard", idx: sh, closeDB: closeSh}, &opened, &reloadClosers)
		}
	}

	if shardsDir != "" {
		monthly, err := discoverMonthlyShards(shardsDir)
		if err != nil {
			log.Warn("reload shards-dir échoué", "dir", shardsDir, "err", err.Error())
		} else {
			for _, m := range monthly {
				idx, closeDB, err := openIndex(m.indexPath, m.arenaPath)
				if err != nil {
					log.Warn("reload arène mensuelle illisible, ignorée", "label", m.label, "err", err.Error())
					continue
				}
				appendOpened(openedIndex{label: m.label, idx: idx, closeDB: closeDB}, &opened, &reloadClosers)
			}
		}
	}

	members := make([]labeledSearcher, len(opened))
	labels := make([]string, len(opened))
	for i, o := range opened {
		members[i] = labeledSearcher{label: o.label, s: o.idx}
		labels[i] = o.label
	}
	return newFederatedSearcher(log, members), reloadClosers, labels
}
