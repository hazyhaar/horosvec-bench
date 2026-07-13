// hnbook-titles gère le magasin SQLite de titres HackerNews (voie deux-temps :
// chargement massif via duckdb/bulk_convert.sh, delta via append).
package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	_ "modernc.org/sqlite"
)

const (
	driverName     = "sqlite"
	batchSize      = 1000
	maxWalkDepth   = 64
	insertItemStmt = `INSERT OR REPLACE INTO item(id, ts, type, title, parent, root_id, depth, orphan, text) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)`
)

const schemaDDL = `
CREATE TABLE IF NOT EXISTS item(
  id INTEGER PRIMARY KEY,
  ts INTEGER,
  type TEXT,
  title TEXT,
  parent INTEGER,
  root_id INTEGER,
  depth INTEGER,
  orphan INTEGER DEFAULT 0,
  text TEXT
);
CREATE INDEX IF NOT EXISTS idx_item_root_id ON item(root_id);
CREATE INDEX IF NOT EXISTS idx_item_parent ON item(parent);
CREATE TABLE IF NOT EXISTS pending(
  id INTEGER PRIMARY KEY,
  parent INTEGER,
  ts INTEGER,
  type TEXT,
  title TEXT,
  text TEXT
);
`

type itemRow struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Parent int64  `json:"parent"`
	Text   string `json:"text"`
}

type treeFields struct {
	RootID  int64
	Depth   int
	Orphan  int
	HasTree bool
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "-h", "--help", "help":
		printUsage()
		return
	case "init":
		runInit(os.Args[2:])
	case "append":
		runAppend(os.Args[2:])
	case "stat":
		runStat(os.Args[2:])
	case "backfill-tree":
		runBackfillTree(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `hnbook-titles — magasin SQLite de titres HackerNews

usage:
  hnbook-titles init           -db <path>
  hnbook-titles append         -db <path> -in <ndjson|->
  hnbook-titles stat           -db <path>
  hnbook-titles backfill-tree  -db <path>

`)
}

func runInit(args []string) {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	dbPath := fs.String("db", "", "chemin du fichier SQLite")
	fs.Parse(args)

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "init: -db is required")
		os.Exit(2)
	}

	ctx := context.Background()
	if err := initDB(ctx, *dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		os.Exit(1)
	}
}

func runAppend(args []string) {
	fs := flag.NewFlagSet("append", flag.ExitOnError)
	dbPath := fs.String("db", "", "chemin du fichier SQLite")
	inPath := fs.String("in", "", "fichier NDJSON ou - pour stdin")
	fs.Parse(args)

	if *dbPath == "" || *inPath == "" {
		fmt.Fprintln(os.Stderr, "append: -db and -in are required")
		os.Exit(2)
	}

	var in io.Reader
	if *inPath == "-" {
		in = os.Stdin
	} else {
		f, err := os.Open(*inPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "append: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		in = f
	}

	ctx := context.Background()
	if err := appendNDJSON(ctx, *dbPath, in); err != nil {
		fmt.Fprintf(os.Stderr, "append: %v\n", err)
		os.Exit(1)
	}
}

func runStat(args []string) {
	fs := flag.NewFlagSet("stat", flag.ExitOnError)
	dbPath := fs.String("db", "", "chemin du fichier SQLite")
	fs.Parse(args)

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "stat: -db is required")
		os.Exit(2)
	}

	ctx := context.Background()
	if err := printStat(ctx, *dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "stat: %v\n", err)
		os.Exit(1)
	}
}

func runBackfillTree(args []string) {
	fs := flag.NewFlagSet("backfill-tree", flag.ExitOnError)
	dbPath := fs.String("db", "", "chemin du fichier SQLite")
	fs.Parse(args)

	if *dbPath == "" {
		fmt.Fprintln(os.Stderr, "backfill-tree: -db is required")
		os.Exit(2)
	}

	ctx := context.Background()
	if err := backfillTree(ctx, *dbPath); err != nil {
		fmt.Fprintf(os.Stderr, "backfill-tree: %v\n", err)
		os.Exit(1)
	}
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open(driverName, path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func initDB(ctx context.Context, path string) error {
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, schemaDDL); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	if err := migrateTextColumn(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL`); err != nil {
		return fmt.Errorf("pragma journal_mode: %w", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout=5000`); err != nil {
		return fmt.Errorf("pragma busy_timeout: %w", err)
	}
	return nil
}

func lookupParentTree(ctx context.Context, q rowQuerier, parentID int64) (treeFields, bool, error) {
	var rootID sql.NullInt64
	var depth sql.NullInt64
	var orphan sql.NullInt64
	err := q.QueryRowContext(ctx,
		`SELECT root_id, depth, orphan FROM item WHERE id = ?`, parentID,
	).Scan(&rootID, &depth, &orphan)
	if err == sql.ErrNoRows {
		return treeFields{}, false, nil
	}
	if err != nil {
		return treeFields{}, false, err
	}
	if !rootID.Valid {
		return treeFields{}, true, nil
	}
	return treeFields{
		RootID:  rootID.Int64,
		Depth:   int(depth.Int64),
		Orphan:  int(orphan.Int64),
		HasTree: true,
	}, true, nil
}

func deriveTreeFields(id, parent int64, parentTree treeFields) treeFields {
	if parent == 0 {
		return treeFields{RootID: id, Depth: 0, Orphan: 0, HasTree: true}
	}
	if parentTree.Orphan != 0 {
		return treeFields{RootID: id, Depth: 0, Orphan: 1, HasTree: true}
	}
	return treeFields{
		RootID:  parentTree.RootID,
		Depth:   parentTree.Depth + 1,
		Orphan:  0,
		HasTree: true,
	}
}

func migrateTextColumn(ctx context.Context, db *sql.DB) error {
	for _, stmt := range []string{
		`ALTER TABLE item ADD COLUMN text TEXT`,
		`ALTER TABLE pending ADD COLUMN text TEXT`,
	} {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column") {
				continue
			}
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func insertItem(ctx context.Context, tx *sql.Tx, row itemRow, tree treeFields) error {
	_, err := tx.ExecContext(ctx, insertItemStmt,
		row.ID, row.TS, row.Type, row.Title, row.Parent,
		tree.RootID, tree.Depth, tree.Orphan, row.Text,
	)
	return err
}

func enqueuePending(ctx context.Context, tx *sql.Tx, row itemRow) error {
	_, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO pending(id, parent, ts, type, title, text) VALUES(?, ?, ?, ?, ?, ?)`,
		row.ID, row.Parent, row.TS, row.Type, row.Title, row.Text,
	)
	return err
}

func resolvePending(ctx context.Context, db *sql.DB) error {
	for {
		rows, err := db.QueryContext(ctx, `SELECT id, parent, ts, type, title, coalesce(text,'') FROM pending`)
		if err != nil {
			return err
		}

		type pendingRow struct {
			itemRow
		}
		var pending []pendingRow
		for rows.Next() {
			var r pendingRow
			if err := rows.Scan(&r.ID, &r.Parent, &r.TS, &r.Type, &r.Title, &r.Text); err != nil {
				_ = rows.Close()
				return err
			}
			pending = append(pending, r)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}

		var resolved []int64
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, row := range pending {
			parentTree, found, err := lookupParentTree(ctx, tx, row.Parent)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if !found || !parentTree.HasTree {
				continue
			}
			tree := deriveTreeFields(row.ID, row.Parent, parentTree)
			if err := insertItem(ctx, tx, row.itemRow, tree); err != nil {
				_ = tx.Rollback()
				return err
			}
			resolved = append(resolved, row.ID)
		}
		for _, id := range resolved {
			if _, err := tx.ExecContext(ctx, `DELETE FROM pending WHERE id = ?`, id); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if len(resolved) == 0 {
			return nil
		}
	}
}

func appendNDJSON(ctx context.Context, path string, in io.Reader) error {
	if err := initDB(ctx, path); err != nil {
		return err
	}

	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer db.Close()

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var batch []itemRow
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, row := range batch {
			if row.Parent == 0 {
				tree := deriveTreeFields(row.ID, 0, treeFields{})
				if err := insertItem(ctx, tx, row, tree); err != nil {
					_ = tx.Rollback()
					return err
				}
				continue
			}
			parentTree, found, err := lookupParentTree(ctx, tx, row.Parent)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if !found || !parentTree.HasTree {
				if err := enqueuePending(ctx, tx, row); err != nil {
					_ = tx.Rollback()
					return err
				}
				continue
			}
			tree := deriveTreeFields(row.ID, row.Parent, parentTree)
			if err := insertItem(ctx, tx, row, tree); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		batch = batch[:0]
		return resolvePending(ctx, db)
	}

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row itemRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return fmt.Errorf("decode line: %w", err)
		}
		batch = append(batch, row)
		if len(batch) >= batchSize {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	return resolvePending(ctx, db)
}

type backfillState struct {
	maxID    int64
	count    int64
	exists   []bool
	parents  []int64
	roots    []int64
	depths   []int
	resolved []bool
	orphans  []bool
}

func loadBackfillState(ctx context.Context, db *sql.DB) (*backfillState, error) {
	var maxID sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT max(id) FROM item`).Scan(&maxID); err != nil {
		return nil, err
	}
	if !maxID.Valid || maxID.Int64 == 0 {
		return &backfillState{}, nil
	}

	st := &backfillState{maxID: maxID.Int64}
	n := int(maxID.Int64) + 1
	st.exists = make([]bool, n)
	st.parents = make([]int64, n)
	st.roots = make([]int64, n)
	st.depths = make([]int, n)
	st.resolved = make([]bool, n)
	st.orphans = make([]bool, n)

	rows, err := db.QueryContext(ctx, `SELECT id, parent FROM item ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id, parent int64
		if err := rows.Scan(&id, &parent); err != nil {
			return nil, err
		}
		st.exists[id] = true
		st.parents[id] = parent
		st.count++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ramBytes := int64(n)*8 + int64(n)*8 + int64(n)*8 // exists bitmap approx + parents + roots
	_ = ramBytes
	fmt.Fprintf(os.Stderr, "backfill-tree: items=%d max_id=%d ram_arrays_mb=%.1f\n",
		st.count, st.maxID, float64(int64(n)*24)/1024/1024)

	return st, nil
}

func backfillPass1(st *backfillState) (deferred []int64) {
	for id := int64(1); id <= st.maxID; id++ {
		if !st.exists[id] {
			continue
		}
		parent := st.parents[id]
		if parent == 0 {
			st.roots[id] = id
			st.depths[id] = 0
			st.resolved[id] = true
			continue
		}
		if parent > st.maxID || parent < 0 || !st.exists[parent] {
			deferred = append(deferred, id)
			continue
		}
		if st.resolved[parent] {
			if st.orphans[parent] {
				st.roots[id] = id
				st.depths[id] = 0
				st.orphans[id] = true
				st.resolved[id] = true
				continue
			}
			st.roots[id] = st.roots[parent]
			st.depths[id] = st.depths[parent] + 1
			st.resolved[id] = true
			continue
		}
		deferred = append(deferred, id)
	}
	return deferred
}

func backfillResolveDeferred(st *backfillState, deferred []int64) (orphanCount, anomalyCount int64) {
	for _, id := range deferred {
		if st.resolved[id] {
			continue
		}
		root, depth, orphan, reason := walkResolve(st, id)
		if orphan {
			st.roots[id] = id
			st.depths[id] = 0
			st.orphans[id] = true
			st.resolved[id] = true
			orphanCount++
			fmt.Fprintf(os.Stderr, "backfill-tree: orphan id=%d parent=%d reason=%s\n", id, st.parents[id], reason)
			continue
		}
		st.roots[id] = root
		st.depths[id] = depth
		st.resolved[id] = true
		if st.parents[id] >= id {
			anomalyCount++
			fmt.Fprintf(os.Stderr, "backfill-tree: resolved anomaly parent>=id id=%d parent=%d root=%d depth=%d\n",
				id, st.parents[id], root, depth)
		}
	}
	return orphanCount, anomalyCount
}

func walkResolve(st *backfillState, start int64) (root int64, depth int, orphan bool, reason string) {
	visited := make(map[int64]bool)
	cur := start
	steps := 0
	for {
		parent := st.parents[cur]
		if parent == 0 {
			if !st.exists[cur] {
				return start, 0, true, "root_missing"
			}
			return cur, steps, false, ""
		}
		if parent > st.maxID || parent < 0 || !st.exists[parent] {
			return start, 0, true, "parent_absent"
		}
		if visited[parent] {
			return start, 0, true, "cycle"
		}
		visited[parent] = true
		if st.resolved[parent] {
			if st.orphans[parent] {
				return start, 0, true, "parent_orphan"
			}
			return st.roots[parent], st.depths[parent] + steps + 1, false, ""
		}
		if steps >= maxWalkDepth {
			return start, 0, true, "depth_cap"
		}
		cur = parent
		steps++
	}
}

func applyBackfillUpdates(ctx context.Context, db *sql.DB, st *backfillState) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `UPDATE item SET root_id=?, depth=?, orphan=? WHERE id=?`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()

	var batch int
	for id := int64(1); id <= st.maxID; id++ {
		if !st.exists[id] {
			continue
		}
		orphan := 0
		if st.orphans[id] {
			orphan = 1
		}
		if _, err := stmt.ExecContext(ctx, st.roots[id], st.depths[id], orphan, id); err != nil {
			_ = tx.Rollback()
			return err
		}
		batch++
		if batch >= batchSize {
			if err := tx.Commit(); err != nil {
				return err
			}
			tx, err = db.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			stmt, err = tx.PrepareContext(ctx, `UPDATE item SET root_id=?, depth=?, orphan=? WHERE id=?`)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			batch = 0
		}
	}
	return tx.Commit()
}

func backfillTree(ctx context.Context, path string) error {
	if err := initDB(ctx, path); err != nil {
		return err
	}
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer db.Close()

	var before int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM item`).Scan(&before); err != nil {
		return err
	}

	st, err := loadBackfillState(ctx, db)
	if err != nil {
		return err
	}
	if st.count == 0 {
		fmt.Println("backfill-tree: empty store")
		return nil
	}

	deferred := backfillPass1(st)
	fmt.Fprintf(os.Stderr, "backfill-tree: pass1 resolved=%d deferred=%d\n",
		st.count-int64(len(deferred)), len(deferred))

	orphans, anomalies := backfillResolveDeferred(st, deferred)
	fmt.Fprintf(os.Stderr, "backfill-tree: pass2 orphans=%d anomalies_resolved=%d\n", orphans, anomalies)

	if err := applyBackfillUpdates(ctx, db, st); err != nil {
		return err
	}

	var after int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM item`).Scan(&after); err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("count changed: before=%d after=%d", before, after)
	}

	var threads, orphanTotal, maxDepth int64
	_ = db.QueryRowContext(ctx, `SELECT count(DISTINCT root_id) FROM item WHERE orphan=0`).Scan(&threads)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM item WHERE orphan=1`).Scan(&orphanTotal)
	_ = db.QueryRowContext(ctx, `SELECT coalesce(max(depth),0) FROM item WHERE orphan=0`).Scan(&maxDepth)

	fmt.Printf("backfill-tree: count=%d threads=%d orphans=%d max_depth=%d\n",
		after, threads, orphanTotal, maxDepth)
	return nil
}

func printStat(ctx context.Context, path string) error {
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer db.Close()

	var count int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM item`).Scan(&count); err != nil {
		return err
	}

	var minTS, maxTS sql.NullInt64
	if err := db.QueryRowContext(ctx, `SELECT min(ts), max(ts) FROM item`).Scan(&minTS, &maxTS); err != nil {
		return err
	}

	var dupes int64
	dupQuery := `SELECT count(*) FROM (SELECT id FROM item GROUP BY id HAVING count(*)>1)`
	if err := db.QueryRowContext(ctx, dupQuery).Scan(&dupes); err != nil {
		return err
	}

	var threads, orphans, maxDepth, pending int64
	_ = db.QueryRowContext(ctx, `SELECT count(DISTINCT root_id) FROM item WHERE orphan=0`).Scan(&threads)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM item WHERE orphan=1`).Scan(&orphans)
	_ = db.QueryRowContext(ctx, `SELECT coalesce(max(depth),0) FROM item`).Scan(&maxDepth)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM pending`).Scan(&pending)

	fmt.Printf("count=%d\n", count)
	if minTS.Valid {
		fmt.Printf("min_ts=%d\n", minTS.Int64)
	} else {
		fmt.Println("min_ts=NULL")
	}
	if maxTS.Valid {
		fmt.Printf("max_ts=%d\n", maxTS.Int64)
	} else {
		fmt.Println("max_ts=NULL")
	}
	fmt.Printf("duplicates=%d\n", dupes)
	fmt.Printf("threads=%d\n", threads)
	fmt.Printf("orphans=%d\n", orphans)
	fmt.Printf("max_depth=%d\n", maxDepth)
	fmt.Printf("pending=%d\n", pending)
	return nil
}
