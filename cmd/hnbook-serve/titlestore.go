package main

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// storeItem est une ligne item du magasin SQLite pour un hit vectoriel.
type storeItem struct {
	ID     int64
	Type   string
	Title  string
	Text   string
	RootID int64
	Depth  int
	Parent int64
}

// itemDetail porte les champs servis par /api/item.
type itemDetail struct {
	ID    int64
	Title string
	Text  string
	Type  string
	TS    int64
}

// rootInfo porte le titre et l'horodatage d'une story racine (root_id).
type rootInfo struct {
	ID    int64
	Title string
	TS    int64
}

// titleStore ouvre le magasin arborescent en lecture seule (modernc, sans ATTACH).
type titleStore struct {
	db        *sql.DB
	maxTS     atomic.Int64
	batchHits atomic.Int32 // compteur de requêtes batch hits (tests uniquement)
	batchRoot atomic.Int32 // compteur de requêtes batch racines (tests uniquement)
}

// openTitleStore ouvre la SQLite en mode lecture seule et pré-calcule max(ts).
func openTitleStore(path string) (*titleStore, error) {
	dsn := path + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open title store: %w", err)
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var maxTS sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max(ts) FROM item`).Scan(&maxTS); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("freshness query: %w", err)
	}
	s := &titleStore{db: db}
	if maxTS.Valid {
		s.maxTS.Store(maxTS.Int64)
	}
	return s, nil
}

func (s *titleStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *titleStore) FreshnessTS() int64 {
	if s == nil {
		return 0
	}
	return s.maxTS.Load()
}

// refreshFreshness recalcule max(ts) depuis le magasin (SELECT indexé, peu coûteux).
func (s *titleStore) refreshFreshness(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	var maxTS sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT max(ts) FROM item`).Scan(&maxTS); err != nil {
		return fmt.Errorf("freshness refresh: %w", err)
	}
	if maxTS.Valid {
		s.maxTS.Store(maxTS.Int64)
	} else {
		s.maxTS.Store(0)
	}
	return nil
}

// fetchItemsByIDs charge en UNE requête les métadonnées des hits (jamais N lookups).
func (s *titleStore) fetchItemsByIDs(ctx context.Context, ids []int64) (map[int64]storeItem, error) {
	s.batchHits.Add(1)
	out := make(map[int64]storeItem, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	ph, args := inPlaceholders(ids)
	q := `SELECT id, type, title, coalesce(text,''), root_id, depth, parent FROM item WHERE id IN (` + ph + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch items: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var it storeItem
		if err := rows.Scan(&it.ID, &it.Type, &it.Title, &it.Text, &it.RootID, &it.Depth, &it.Parent); err != nil {
			return nil, fmt.Errorf("scan item: %w", err)
		}
		out[it.ID] = it
	}
	return out, rows.Err()
}

// itemByID charge un item par identifiant depuis le magasin lecture seule.
func (s *titleStore) itemByID(ctx context.Context, id int64) (itemDetail, bool, error) {
	var it itemDetail
	err := s.db.QueryRowContext(ctx,
		`SELECT id, title, coalesce(text,''), type, ts FROM item WHERE id = ?`, id,
	).Scan(&it.ID, &it.Title, &it.Text, &it.Type, &it.TS)
	if err == sql.ErrNoRows {
		return itemDetail{}, false, nil
	}
	if err != nil {
		return itemDetail{}, false, fmt.Errorf("item by id: %w", err)
	}
	return it, true, nil
}

// fetchRootsByIDs charge en UNE requête les titres/ts des root_id.
func (s *titleStore) fetchRootsByIDs(ctx context.Context, rootIDs []int64) (map[int64]rootInfo, error) {
	s.batchRoot.Add(1)
	out := make(map[int64]rootInfo, len(rootIDs))
	if len(rootIDs) == 0 {
		return out, nil
	}
	ph, args := inPlaceholders(rootIDs)
	q := `SELECT id, title, ts FROM item WHERE id IN (` + ph + `)`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("fetch roots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r rootInfo
		if err := rows.Scan(&r.ID, &r.Title, &r.TS); err != nil {
			return nil, fmt.Errorf("scan root: %w", err)
		}
		out[r.ID] = r
	}
	return out, rows.Err()
}

func inPlaceholders(ids []int64) (string, []any) {
	parts := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		parts[i] = "?"
		args[i] = id
	}
	return strings.Join(parts, ","), args
}

func parseHitID(raw []byte) (int64, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

func hnItemURL(id int64) string {
	return fmt.Sprintf("https://news.ycombinator.com/item?id=%d", id)
}

// formatHNDate rend une date courte lisible à partir d'un epoch Unix (secondes).
func formatHNDate(ts int64) string {
	if ts <= 0 {
		return ""
	}
	t := time.Unix(ts, 0).UTC()
	months := [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	return fmt.Sprintf("%d %s %d", t.Day(), months[t.Month()-1], t.Year())
}
