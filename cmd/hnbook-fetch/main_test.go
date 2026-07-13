package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestCursorsPersistedAndReloaded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cursors.db")
	ctx := context.Background()

	cs, err := openCursorStore(path)
	if err != nil {
		t.Fatal(err)
	}
	head, backfill, err := cs.initIfMissing(ctx, 44000000, 28700000)
	if err != nil {
		t.Fatal(err)
	}
	if head != 44000000 || backfill != 28700000 {
		t.Fatalf("init: head=%d backfill=%d", head, backfill)
	}
	if err := cs.advance(ctx, cursorHead, 44000005); err != nil {
		t.Fatal(err)
	}
	if err := cs.advance(ctx, cursorBackfill, 28700010); err != nil {
		t.Fatal(err)
	}
	_ = cs.Close()

	cs2, err := openCursorStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	head, backfill, err = cs2.readBoth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if head != 44000005 || backfill != 28700010 {
		t.Fatalf("reload: head=%d backfill=%d", head, backfill)
	}
}

func TestDeadItemAdvancesCursor(t *testing.T) {
	env := newTestEnv(t, testEnvConfig{
		maxItem:       100,
		headStart:     97,
		backfillStart: 50,
		items: map[int64]*hnItem{
			98:  {ID: 98, Type: "story", Time: 1, Title: "live", Parent: 0},
			99:  {ID: 99, Dead: true},
			100: {ID: 100, Type: "comment", Time: 2, Title: "c", Text: "body", Parent: 98},
		},
		budget: "10",
	})
	defer env.close()

	headBefore, _ := env.cursors()
	if err := env.runOnce(); err != nil {
		t.Fatal(err)
	}
	headAfter, _ := env.cursors()
	if headBefore != 97 {
		t.Fatalf("head before=%d", headBefore)
	}
	if headAfter != 100 {
		t.Fatalf("head after=%d want 100 (dead 99 skipped but advanced)", headAfter)
	}
}

func TestIdempotenceReplayAddsNothing(t *testing.T) {
	env := newTestEnv(t, testEnvConfig{
		maxItem:       101,
		headStart:     100,
		backfillStart: 50,
		items: map[int64]*hnItem{
			101: {ID: 101, Type: "story", Time: 3, Title: "once", Text: "hello", Parent: 0},
		},
		budget: "100",
	})
	defer env.close()

	if err := env.runOnce(); err != nil {
		t.Fatal(err)
	}
	n1, err := env.shardCount()
	if err != nil {
		t.Fatal(err)
	}
	if n1 != 1 {
		t.Fatalf("shard count=%d want 1", n1)
	}
	if err := env.runOnce(); err != nil {
		t.Fatal(err)
	}
	n2, err := env.shardCount()
	if err != nil {
		t.Fatal(err)
	}
	if n2 != n1 {
		t.Fatalf("idempotence: count before=%d after=%d", n1, n2)
	}
}

func TestBackfillBudgetStops(t *testing.T) {
	items := map[int64]*hnItem{
		51: {ID: 51, Type: "story", Time: 1, Title: "a", Parent: 0},
		52: {ID: 52, Type: "story", Time: 2, Title: "b", Parent: 0},
		53: {ID: 53, Type: "story", Time: 3, Title: "c", Parent: 0},
	}
	env := newTestEnv(t, testEnvConfig{
		maxItem:       60,
		headStart:     60,
		backfillStart: 50,
		items:         items,
		budget:        "2",
	})
	defer env.close()

	if err := env.runOnce(); err != nil {
		t.Fatal(err)
	}
	_, back := env.cursors()
	if back != 52 {
		t.Fatalf("backfill=%d want 52 (budget 2 items)", back)
	}
}

func TestHeadFloorNeverCrossed(t *testing.T) {
	items := map[int64]*hnItem{
		55: {ID: 55, Type: "story", Time: 1, Title: "old", Parent: 0},
		60: {ID: 60, Type: "story", Time: 2, Title: "at floor", Parent: 0},
		61: {ID: 61, Type: "story", Time: 3, Title: "past floor", Parent: 0},
	}
	env := newTestEnv(t, testEnvConfig{
		maxItem:       61,
		headStart:     59,
		backfillStart: 54,
		items:         items,
		budget:        "100",
	})
	defer env.close()

	if err := env.runOnce(); err != nil {
		t.Fatal(err)
	}
	_, back := env.cursors()
	if back != 59 {
		t.Fatalf("backfill=%d want 59 (stopped before head_floor 60)", back)
	}
	count, _ := env.shardCount()
	if count != 3 {
		t.Fatalf("shard count=%d want 3 (55 backfill + 60,61 head; id 60 jamais via rattrapage)", count)
	}
}

func TestDoubleWriteEmbedFailure(t *testing.T) {
	var failEmbed atomic.Bool
	failEmbed.Store(true)

	env := newTestEnv(t, testEnvConfig{
		maxItem:       200,
		headStart:     199,
		backfillStart: 100,
		items: map[int64]*hnItem{
			200: {ID: 200, Type: "story", Time: 1, Title: "fragile", Text: "x", Parent: 0},
		},
		budget:    "5",
		embedFail: &failEmbed,
	})
	defer env.close()

	err := env.runOnce()
	if err == nil {
		t.Fatal("expected embed failure")
	}
	head, _ := env.cursors()
	if head != 199 {
		t.Fatalf("head=%d want 199 (not advanced on embed failure)", head)
	}
	n, _ := env.shardCount()
	if n != 0 {
		t.Fatalf("shard count=%d want 0", n)
	}
	storeN, _ := env.storeCount()
	if storeN != 0 {
		t.Fatalf("store count=%d want 0", storeN)
	}
}

func TestSimulatedRunNoArenaTouched(t *testing.T) {
	items := map[int64]*hnItem{
		28800001: {ID: 28800001, Type: "story", Time: 10, Title: "bf", Text: "back", Parent: 0},
		44000001: {ID: 44000001, Type: "story", Time: 11, Title: "head", Text: "fresh", Parent: 0},
	}
	env := newTestEnv(t, testEnvConfig{
		maxItem:       44000001,
		headStart:     44000000,
		backfillStart: 28800000,
		items:         items,
		budget:        "1",
	})
	defer env.close()

	arenaPath := filepath.Join(env.dir, "sentinel.arena")
	if err := os.WriteFile(arenaPath, []byte("ARENA"), 0o644); err != nil {
		t.Fatal(err)
	}
	beforeArena := fileStatModTime(t, arenaPath)

	headB, backB := env.cursors()
	t.Logf("curseurs avant: head=%d backfill=%d", headB, backB)

	if err := env.runOnce(); err != nil {
		t.Fatal(err)
	}

	headA, backA := env.cursors()
	count, _ := env.shardCount()
	t.Logf("curseurs après: head=%d backfill=%d shard_nodes=%d", headA, backA, count)

	if headA != 44000001 {
		t.Fatalf("head=%d want 44000001", headA)
	}
	if backA != 28800001 {
		t.Fatalf("backfill=%d want 28800001", backA)
	}
	if count != 2 {
		t.Fatalf("shard count=%d want 2", count)
	}
	if fileStatModTime(t, arenaPath) != beforeArena {
		t.Fatal("sentinel arena modtime changed")
	}
}

func TestRunLogSuccess(t *testing.T) {
	env := newTestEnv(t, testEnvConfig{
		maxItem:       101,
		headStart:     100,
		backfillStart: 50,
		items: map[int64]*hnItem{
			101: {ID: 101, Type: "story", Time: 1, Title: "ok", Parent: 0},
		},
		budget: "100",
	})
	defer env.close()

	if _, err := env.runner.runCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := env.lastRuns(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("runs=%d want 1", len(rows))
	}
	r := rows[0]
	if r.Status != "ok" {
		t.Fatalf("status=%q want ok", r.Status)
	}
	if r.HeadIngested != 1 {
		t.Fatalf("head_ingested=%d want 1", r.HeadIngested)
	}
	if r.Error.Valid && r.Error.String != "" {
		t.Fatalf("error=%v want empty", r.Error)
	}
}

func TestRunLogErrorAndResume(t *testing.T) {
	var failFetch atomic.Bool
	failFetch.Store(true)

	env := newTestEnv(t, testEnvConfig{
		maxItem:       201,
		headStart:     199,
		backfillStart: 100,
		items: map[int64]*hnItem{
			200: {ID: 200, Type: "story", Time: 1, Title: "fragile", Text: "x", Parent: 0},
			201: {ID: 201, Type: "story", Time: 2, Title: "next", Text: "y", Parent: 0},
		},
		budget:    "5",
		embedFail: &failFetch,
	})
	defer env.close()

	_, err := env.runner.runCycle(context.Background())
	if err == nil {
		t.Fatal("expected embed failure")
	}
	if !strings.Contains(err.Error(), "head id=200") {
		t.Fatalf("error=%q want head id=200", err.Error())
	}
	head, _ := env.cursors()
	if head != 199 {
		t.Fatalf("head=%d want 199 (not advanced on failure)", head)
	}
	rows, err := env.lastRuns(1)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != "error" {
		t.Fatalf("status=%q want error", rows[0].Status)
	}
	if !strings.Contains(rows[0].Error.String, "id=200") {
		t.Fatalf("run_log error=%q want id=200", rows[0].Error.String)
	}

	failFetch.Store(false)
	if _, err := env.runner.runCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	head, _ = env.cursors()
	if head != 201 {
		t.Fatalf("head=%d want 201 after resume", head)
	}
	rows, err = env.lastRuns(1)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != "ok" {
		t.Fatalf("resume status=%q want ok", rows[0].Status)
	}
}

func TestMaxHeadLimits(t *testing.T) {
	items := map[int64]*hnItem{}
	for id := int64(101); id <= 105; id++ {
		items[id] = &hnItem{ID: id, Type: "story", Time: id, Title: fmt.Sprintf("s%d", id), Parent: 0}
	}
	env := newTestEnv(t, testEnvConfig{
		maxItem:       105,
		headStart:     100,
		backfillStart: 50,
		items:         items,
		budget:        "100",
		maxHead:       2,
	})
	defer env.close()

	if _, err := env.runner.runCycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	head, _ := env.cursors()
	if head != 102 {
		t.Fatalf("head=%d want 102 (max-head 2)", head)
	}
}

func TestAppendItemRetryOnLocked(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(dir, "count")
	scriptPath := filepath.Join(dir, "fake-titles.sh")
	script := fmt.Sprintf(`#!/bin/sh
count=$(cat %q 2>/dev/null || echo 0)
count=$((count + 1))
echo "$count" > %q
if [ "$count" -lt 3 ]; then
  echo "database is locked" >&2
  exit 1
fi
exit 0
`, counterFile, counterFile)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a := titlesAppender{bin: scriptPath}
	if err := a.appendItem(context.Background(), filepath.Join(dir, "store.db"), `{"id":1}`); err != nil {
		t.Fatalf("appendItem: %v", err)
	}
	data, err := os.ReadFile(counterFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "3" {
		t.Fatalf("attempts=%q want 3", data)
	}
}

func TestAppendItemNoRetryOnOtherError(t *testing.T) {
	dir := t.TempDir()
	counterFile := filepath.Join(dir, "count")
	scriptPath := filepath.Join(dir, "fail-titles.sh")
	script := fmt.Sprintf(`#!/bin/sh
echo 1 > %q
echo "permission denied" >&2
exit 1
`, counterFile)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	a := titlesAppender{bin: scriptPath}
	err := a.appendItem(context.Background(), filepath.Join(dir, "store.db"), `{"id":1}`)
	if err == nil {
		t.Fatal("expected error")
	}
	if isSQLiteContention(err) {
		t.Fatalf("should not classify as contention: %v", err)
	}
	data, _ := os.ReadFile(counterFile)
	if strings.TrimSpace(string(data)) != "1" {
		t.Fatalf("attempts=%q want 1 (no retry)", data)
	}
}

func TestLogFileJSON(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "fetch.log")

	env := newTestEnv(t, testEnvConfig{
		maxItem:       101,
		headStart:     100,
		backfillStart: 50,
		items: map[int64]*hnItem{
			101: {ID: 101, Type: "story", Time: 1, Title: "logme", Parent: 0},
		},
		budget: "100",
	})
	defer env.close()

	if err := setupLogging(logPath); err != nil {
		t.Fatal(err)
	}
	if _, err := env.runner.runCycle(context.Background()); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) < 1 {
		t.Fatal("log file empty")
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("not JSON slog: %v line=%q", err, lines[0])
	}
	if _, ok := entry["msg"]; !ok {
		t.Fatalf("missing msg field: %v", entry)
	}
}

func TestRejectArenaShardPath(t *testing.T) {
	for _, p := range []string{"", "/tmp/foo.arena", "/data/monolith.arena.ids"} {
		if err := rejectArenaShardPath(p); err == nil {
			t.Fatalf("path %q should be rejected", p)
		}
	}
}

type testEnvConfig struct {
	maxItem       int64
	headStart     int64
	backfillStart int64
	items         map[int64]*hnItem
	budget        string
	maxHead       int
	embedFail     *atomic.Bool
}

type testEnv struct {
	dir    string
	runner *runner
}

func newTestEnv(t *testing.T, cfg testEnvConfig) *testEnv {
	t.Helper()
	dir := t.TempDir()

	titlesBin := filepath.Join("/devhoros/horosvec-bench", "bin", "hnbook-titles")
	if _, err := os.Stat(titlesBin); err != nil {
		t.Fatalf("hnbook-titles binary missing: %v", err)
	}

	storePath := filepath.Join(dir, "store.db")
	ctx := context.Background()
	initOut, err := exec.Command(titlesBin, "init", "-db", storePath).CombinedOutput()
	if err != nil {
		t.Fatalf("titles init: %v %s", err, initOut)
	}

	shardPath := filepath.Join(dir, "shard.db")
	cursorsPath := filepath.Join(dir, "cursors.db")

	embedFail := cfg.embedFail
	if embedFail == nil {
		embedFail = &atomic.Bool{}
	}

	hnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/maxitem.json"):
			fmt.Fprint(w, cfg.maxItem)
		case strings.Contains(r.URL.Path, "/item/"):
			idStr := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v0/item/"), ".json")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			item, ok := cfg.items[id]
			if !ok {
				fmt.Fprint(w, "null")
				return
			}
			_ = json.NewEncoder(w).Encode(item)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hnSrv.Close)

	embedSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if embedFail != nil && embedFail.Load() {
			http.Error(w, "embed down", http.StatusInternalServerError)
			return
		}
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		vec := make([]float32, embedDim)
		vec[0] = 1
		var norm float64
		for _, v := range vec {
			norm += float64(v) * float64(v)
		}
		inv := float32(1.0 / math.Sqrt(norm))
		for i := range vec {
			vec[i] *= inv
		}
		_ = json.NewEncoder(w).Encode(embedResponse{Vector: vec})
	}))
	t.Cleanup(embedSrv.Close)

	cs, err := openCursorStore(cursorsPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cs.initIfMissing(ctx, cfg.headStart, cfg.backfillStart); err != nil {
		t.Fatal(err)
	}
	_ = cs.Close()

	budget, err := parseBackfillBudget(cfg.budget)
	if err != nil {
		t.Fatal(err)
	}

	runCfg := runConfig{
		cursorsPath:    cursorsPath,
		storePath:      storePath,
		shardPath:      shardPath,
		titlesBin:      titlesBin,
		embedURL:       embedSrv.URL + "/embed",
		hnBaseURL:      hnSrv.URL + "/v0",
		backfillStart:  cfg.backfillStart,
		concurrency:    4,
		backfillBudget: budget,
		maxHead:        cfg.maxHead,
		once:           true,
	}

	r, err := newRunner(runCfg, hnSrv.Client(), embedSrv.Client())
	if err != nil {
		t.Fatal(err)
	}

	return &testEnv{dir: dir, runner: r}
}

func (e *testEnv) close() {
	if e.runner != nil {
		_ = e.runner.Close()
	}
}

func (e *testEnv) runOnce() error {
	_, err := e.runner.runCycle(context.Background())
	return err
}

func (e *testEnv) cursors() (head, backfill int64) {
	h, b, err := e.runner.cursors.readBoth(context.Background())
	if err != nil {
		panic(err)
	}
	return h, b
}

func (e *testEnv) shardCount() (int64, error) {
	return e.runner.shardNodeCount(context.Background())
}

func (e *testEnv) storeCount() (int64, error) {
	db, err := sql.Open("sqlite", e.runner.cfg.storePath)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int64
	err = db.QueryRow(`SELECT count(*) FROM item`).Scan(&n)
	return n, err
}

func (e *testEnv) lastRuns(n int) ([]runLogRow, error) {
	return e.runner.cursors.lastRuns(context.Background(), n)
}

func fileStatModTime(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime()
}
