package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testNDJSON = `{"id":900001,"ts":1160418091,"type":"story","title":"Test item A","parent":0}
{"id":900002,"ts":1160418111,"type":"story","title":"Test item B","parent":0}
{"id":900003,"ts":1160418200,"type":"comment","title":"Test item C","parent":900001}
`

const disjointNDJSON = `{"id":900004,"ts":1160418300,"type":"story","title":"Test item D","parent":0}
`

func TestInitAppendStat(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "titles.db")

	ctx := context.Background()
	if err := initDB(ctx, dbPath); err != nil {
		t.Fatalf("initDB: %v", err)
	}

	if err := appendNDJSON(ctx, dbPath, strings.NewReader(testNDJSON)); err != nil {
		t.Fatalf("appendNDJSON first: %v", err)
	}

	out, err := captureStat(ctx, dbPath)
	if err != nil {
		t.Fatalf("stat first: %v", err)
	}
	if !strings.Contains(out, "count=3") {
		t.Fatalf("expected count=3, got:\n%s", out)
	}
	if !strings.Contains(out, "duplicates=0") {
		t.Fatalf("expected duplicates=0, got:\n%s", out)
	}

	if err := appendNDJSON(ctx, dbPath, strings.NewReader(testNDJSON)); err != nil {
		t.Fatalf("appendNDJSON re-append: %v", err)
	}
	out, err = captureStat(ctx, dbPath)
	if err != nil {
		t.Fatalf("stat after re-append: %v", err)
	}
	if !strings.Contains(out, "count=3") {
		t.Fatalf("expected stable count=3 after re-append, got:\n%s", out)
	}

	if err := appendNDJSON(ctx, dbPath, strings.NewReader(disjointNDJSON)); err != nil {
		t.Fatalf("appendNDJSON disjoint: %v", err)
	}
	rows, err := readItems(ctx, dbPath, []int64{900001, 900002, 900003})
	if err != nil {
		t.Fatalf("readItems: %v", err)
	}
	if rows[900001] != "Test item A" || rows[900002] != "Test item B" || rows[900003] != "Test item C" {
		t.Fatalf("first 3 items changed: %#v", rows)
	}
	out, err = captureStat(ctx, dbPath)
	if err != nil {
		t.Fatalf("stat final: %v", err)
	}
	if !strings.Contains(out, "count=4") {
		t.Fatalf("expected count=4 after disjoint append, got:\n%s", out)
	}
}

func TestBackfillTreeAndThreadRead(t *testing.T) {
	ctx := context.Background()
	dbPath := buildSyntheticStore(t, 1200)

	if err := backfillTree(ctx, dbPath); err != nil {
		t.Fatalf("backfillTree: %v", err)
	}

	if err := verifyTreeByWalk(ctx, dbPath); err != nil {
		t.Fatalf("verifyTreeByWalk: %v", err)
	}

	if err := verifyThreadReads(ctx, dbPath, 20); err != nil {
		t.Fatalf("verifyThreadReads: %v", err)
	}

	if err := verifyRootIDIndexUsed(ctx, dbPath); err != nil {
		t.Fatalf("verifyRootIDIndexUsed: %v", err)
	}

	// Idempotent re-run
	var before int64
	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM item`).Scan(&before); err != nil {
		t.Fatalf("count before: %v", err)
	}
	_ = db.Close()

	if err := backfillTree(ctx, dbPath); err != nil {
		t.Fatalf("backfillTree rerun: %v", err)
	}
	db, err = openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB rerun: %v", err)
	}
	var after int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM item`).Scan(&after); err != nil {
		t.Fatalf("count after: %v", err)
	}
	_ = db.Close()
	if before != after {
		t.Fatalf("count not stable: before=%d after=%d", before, after)
	}
}

func TestAppendWithText(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "text.db")
	if err := initDB(ctx, dbPath); err != nil {
		t.Fatalf("initDB: %v", err)
	}

	ndjson := `{"id":910001,"ts":1,"type":"story","title":"root story","parent":0}
{"id":910002,"ts":2,"type":"comment","title":"","parent":910001,"text":"Hello &amp; world"}
`
	if err := appendNDJSON(ctx, dbPath, strings.NewReader(ndjson)); err != nil {
		t.Fatalf("append: %v", err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	var text string
	if err := db.QueryRowContext(ctx, `SELECT text FROM item WHERE id=910002`).Scan(&text); err != nil {
		t.Fatalf("select text: %v", err)
	}
	if text != "Hello &amp; world" {
		t.Fatalf("text = %q, want stored verbatim", text)
	}
}

func TestAppendOrderedAndDisordered(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "append_tree.db")
	if err := initDB(ctx, dbPath); err != nil {
		t.Fatalf("initDB: %v", err)
	}

	storyID := int64(8000001)
	commentID := int64(8000002)
	grandID := int64(8000003)

	ordered := fmt.Sprintf(`{"id":%d,"ts":1,"type":"story","title":"root","parent":0}
{"id":%d,"ts":2,"type":"comment","title":"child","parent":%d}
{"id":%d,"ts":3,"type":"comment","title":"grand","parent":%d}
`, storyID, commentID, storyID, grandID, commentID)

	if err := appendNDJSON(ctx, dbPath, strings.NewReader(ordered)); err != nil {
		t.Fatalf("append ordered: %v", err)
	}
	if err := assertTreeFields(ctx, dbPath, grandID, storyID, 2, 0); err != nil {
		t.Fatalf("ordered: %v", err)
	}

	dir2 := t.TempDir()
	dbPath2 := filepath.Join(dir2, "append_disorder.db")
	if err := initDB(ctx, dbPath2); err != nil {
		t.Fatalf("initDB2: %v", err)
	}
	disordered := fmt.Sprintf(`{"id":%d,"ts":3,"type":"comment","title":"grand","parent":%d}
{"id":%d,"ts":2,"type":"comment","title":"child","parent":%d}
{"id":%d,"ts":1,"type":"story","title":"root","parent":0}
`, grandID, commentID, commentID, storyID, storyID)

	if err := appendNDJSON(ctx, dbPath2, strings.NewReader(disordered)); err != nil {
		t.Fatalf("append disordered: %v", err)
	}
	if err := assertTreeFields(ctx, dbPath2, grandID, storyID, 2, 0); err != nil {
		t.Fatalf("disordered: %v", err)
	}

	var pending int64
	db, err := openDB(dbPath2)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pending`).Scan(&pending); err != nil {
		t.Fatalf("pending: %v", err)
	}
	_ = db.Close()
	if pending != 0 {
		t.Fatalf("expected pending=0 after disordered append, got %d", pending)
	}
}

type synthSpec struct {
	id       int64
	parent   int64
	itemType string
	title    string
}

func buildSyntheticStore(t *testing.T, numThreads int) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "synth.db")
	ctx := context.Background()
	if err := initDB(ctx, dbPath); err != nil {
		t.Fatalf("initDB: %v", err)
	}

	db, err := openDB(dbPath)
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO item(id, ts, type, title, parent) VALUES(?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	base := int64(1_000_000)
	var specs []synthSpec

	for i := 0; i < numThreads; i++ {
		root := base + int64(i*100)
		specs = append(specs, synthSpec{root, 0, "story", fmt.Sprintf("story-%d", i)})
		// linear chain of 3 comments
		c1 := root + 1
		c2 := root + 2
		c3 := root + 3
		specs = append(specs,
			synthSpec{c1, root, "comment", "c1"},
			synthSpec{c2, c1, "comment", "c2"},
			synthSpec{c3, c2, "comment", "c3"},
		)
	}

	// disorder case: parent>=id exception (resolved in pass 2)
	exRoot := base + int64(numThreads*100)
	exChild := exRoot - 5
	specs = append(specs,
		synthSpec{exRoot, 0, "story", "exception-root"},
		synthSpec{exChild, exRoot, "comment", "exception-child"},
	)

	// absent parent orphan
	orphanID := base + int64(numThreads*100) + 50
	specs = append(specs, synthSpec{orphanID, 999999999, "comment", "orphan-absent-parent"})

	// cycle trap: a->b->c->a
	cycleBase := base + int64(numThreads*100) + 100
	specs = append(specs,
		synthSpec{cycleBase, cycleBase + 2, "comment", "cycle-a"},
		synthSpec{cycleBase + 1, cycleBase, "comment", "cycle-b"},
		synthSpec{cycleBase + 2, cycleBase + 1, "comment", "cycle-c"},
	)

	for _, sp := range specs {
		if _, err := stmt.ExecContext(ctx, sp.id, sp.id, sp.itemType, sp.title, sp.parent); err != nil {
			t.Fatalf("insert %d: %v", sp.id, err)
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return dbPath
}

func verifyTreeByWalk(ctx context.Context, dbPath string) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `SELECT id, parent, root_id, depth, orphan FROM item`)
	if err != nil {
		return err
	}
	defer rows.Close()

	parents := make(map[int64]int64)
	stored := make(map[int64]struct {
		rootID int64
		depth  int
		orphan int
	})

	for rows.Next() {
		var id, parent, rootID int64
		var depth, orphan int
		if err := rows.Scan(&id, &parent, &rootID, &depth, &orphan); err != nil {
			return err
		}
		parents[id] = parent
		stored[id] = struct {
			rootID int64
			depth  int
			orphan int
		}{rootID, depth, orphan}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for id, s := range stored {
		if s.orphan == 1 {
			if s.rootID != id {
				return fmt.Errorf("orphan %d: root_id=%d want self", id, s.rootID)
			}
			continue
		}
		expRoot, expDepth, expOrphan, err := independentWalk(id, parents)
		if err != nil {
			return err
		}
		if expOrphan {
			return fmt.Errorf("id %d: expected orphan by walk but orphan=0", id)
		}
		if s.rootID != expRoot || s.depth != expDepth {
			return fmt.Errorf("id %d: stored root=%d depth=%d walk root=%d depth=%d",
				id, s.rootID, s.depth, expRoot, expDepth)
		}
	}
	return nil
}

func independentWalk(id int64, parents map[int64]int64) (root int64, depth int, orphan bool, err error) {
	visited := make(map[int64]bool)
	cur := id
	steps := 0
	for {
		parent, ok := parents[cur]
		if !ok {
			return id, 0, true, nil
		}
		if parent == 0 {
			return cur, steps, false, nil
		}
		if !visited[parent] {
			if _, exists := parents[parent]; !exists {
				return id, 0, true, nil
			}
			visited[parent] = true
			cur = parent
			steps++
			if steps > maxWalkDepth {
				return id, 0, true, nil
			}
			continue
		}
		return id, 0, true, nil
	}
}

func verifyThreadReads(ctx context.Context, dbPath string, minThreads int) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	rootRows, err := db.QueryContext(ctx,
		`SELECT root_id FROM item WHERE orphan=0 AND parent=0 ORDER BY root_id LIMIT ?`, minThreads)
	if err != nil {
		return err
	}
	var roots []int64
	for rootRows.Next() {
		var r int64
		if err := rootRows.Scan(&r); err != nil {
			_ = rootRows.Close()
			return err
		}
		roots = append(roots, r)
	}
	_ = rootRows.Close()
	if len(roots) < minThreads {
		return fmt.Errorf("only %d roots, need %d", len(roots), minThreads)
	}

	for _, root := range roots {
		var threadCount int64
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM item WHERE root_id=? AND orphan=0`, root).Scan(&threadCount); err != nil {
			return err
		}
		var directChildren int64
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM item WHERE parent=? AND orphan=0`, root).Scan(&directChildren); err != nil {
			return err
		}
		if threadCount < 1 || directChildren < 0 {
			return fmt.Errorf("root %d: threadCount=%d directChildren=%d", root, threadCount, directChildren)
		}
		// synthetic trees: root + 3 comments = 4
		if threadCount != 4 {
			return fmt.Errorf("root %d: expected thread size 4, got %d", root, threadCount)
		}
		if directChildren != 1 {
			return fmt.Errorf("root %d: expected 1 direct child, got %d", root, directChildren)
		}
	}
	return nil
}

func verifyRootIDIndexUsed(ctx context.Context, dbPath string) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var sampleRoot int64
	if err := db.QueryRowContext(ctx, `SELECT root_id FROM item WHERE orphan=0 LIMIT 1`).Scan(&sampleRoot); err != nil {
		return err
	}

	rows, err := db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT id FROM item WHERE root_id=?`, sampleRoot)
	if err != nil {
		return err
	}
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		raw := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			return err
		}
		for _, v := range raw {
			if v != nil {
				plan.WriteString(fmt.Sprint(v))
				plan.WriteByte(' ')
			}
		}
		plan.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return err
	}
	text := plan.String()
	if !strings.Contains(text, "idx_item_root_id") && !strings.Contains(strings.ToLower(text), "using index") {
		return fmt.Errorf("EXPLAIN does not use root_id index:\n%s", text)
	}
	return nil
}

func assertTreeFields(ctx context.Context, dbPath string, id, wantRoot int64, wantDepth, wantOrphan int) error {
	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	var rootID int64
	var depth, orphan int
	err = db.QueryRowContext(ctx,
		`SELECT root_id, depth, orphan FROM item WHERE id=?`, id).Scan(&rootID, &depth, &orphan)
	if err != nil {
		return err
	}
	if rootID != wantRoot || depth != wantDepth || orphan != wantOrphan {
		return fmt.Errorf("id %d: got root=%d depth=%d orphan=%d want root=%d depth=%d orphan=%d",
			id, rootID, depth, orphan, wantRoot, wantDepth, wantOrphan)
	}
	return nil
}

func captureStat(ctx context.Context, dbPath string) (string, error) {
	var buf bytes.Buffer
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		return "", err
	}
	os.Stdout = w
	statErr := printStat(ctx, dbPath)
	_ = w.Close()
	os.Stdout = old
	if _, err := buf.ReadFrom(r); err != nil {
		return "", err
	}
	_ = r.Close()
	if statErr != nil {
		return "", statErr
	}
	return buf.String(), nil
}

func readItems(ctx context.Context, dbPath string, ids []int64) (map[int64]string, error) {
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	out := make(map[int64]string, len(ids))
	for _, id := range ids {
		var title string
		err := db.QueryRowContext(ctx, `SELECT title FROM item WHERE id = ?`, id).Scan(&title)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		out[id] = title
	}
	return out, nil
}
