package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hazyhaar/horosvec"
)

type titlesAppender struct {
	bin string
}

func isSQLiteContention(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "locked") || strings.Contains(msg, "busy")
}

func (a titlesAppender) appendItem(ctx context.Context, storePath, line string) error {
	delays := []time.Duration{0, 200 * time.Millisecond, 500 * time.Millisecond}
	var lastErr error
	for attempt, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
		err := a.appendItemOnce(ctx, storePath, line)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isSQLiteContention(err) || attempt == len(delays)-1 {
			return err
		}
	}
	return lastErr
}

func (a titlesAppender) appendItemOnce(ctx context.Context, storePath, line string) error {
	cmd := exec.CommandContext(ctx, a.bin, "append", "-db", storePath, "-in", "-")
	cmd.Stdin = bytes.NewBufferString(line + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("hnbook-titles append: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

type shardIndex struct {
	db  *sql.DB
	idx *horosvec.Index
}

func openShardIndex(path string) (*shardIndex, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	cfg := horosvec.DefaultConfig()
	if cfg.ArenaPath != "" {
		_ = db.Close()
		return nil, fmt.Errorf("refus arène: ArenaPath doit rester vide pour le shard db-blob")
	}
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &shardIndex{db: db, idx: idx}, nil
}

func (s *shardIndex) Close() error {
	if s.idx != nil {
		_ = s.idx.Close()
	}
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *shardIndex) hasExtID(ctx context.Context, extID []byte) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM vindex_nodes WHERE ext_id = ? LIMIT 1`, extID).Scan(&n)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *shardIndex) nodeCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM vindex_nodes`).Scan(&n)
	if err != nil {
		return 0, err
	}
	return n, nil
}

type singleIter struct {
	vec []float32
	id  []byte
	got bool
}

func (it *singleIter) Next() ([]byte, []float32, bool) {
	if it.got {
		return nil, nil, false
	}
	it.got = true
	return it.id, it.vec, true
}

func (it *singleIter) Reset() error {
	it.got = false
	return nil
}

func (s *shardIndex) insertVector(ctx context.Context, extID []byte, vec []float32) error {
	n, err := s.nodeCount(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		iter := &singleIter{id: extID, vec: vec}
		if err := s.idx.Build(ctx, iter); err != nil {
			return fmt.Errorf("horosvec build: %w", err)
		}
		return nil
	}
	if err := s.idx.Insert(ctx, [][]float32{vec}, [][]byte{extID}); err != nil {
		return fmt.Errorf("horosvec insert: %w", err)
	}
	return nil
}

type ingestor struct {
	titles titlesAppender
	embed  *embedClient
	shard  *shardIndex
	store  string
}

func (ing *ingestor) ingestItem(ctx context.Context, item *hnItem) (ingested bool, err error) {
	if item.skip() {
		return false, nil
	}
	extID := hnExtID(item.ID)
	exists, err := ing.shard.hasExtID(ctx, extID)
	if err != nil {
		return false, fmt.Errorf("hasExtID: %w", err)
	}
	if exists {
		return false, nil
	}

	vec, err := ing.embed.embed(ctx, item.embedText())
	if err != nil {
		return false, fmt.Errorf("embed: %w", err)
	}

	if err := ing.titles.appendItem(ctx, ing.store, item.ndjsonLine()); err != nil {
		return false, fmt.Errorf("append: %w", err)
	}

	if err := ing.shard.insertVector(ctx, extID, vec); err != nil {
		return false, fmt.Errorf("insert: %w", err)
	}
	return true, nil
}

func rejectArenaShardPath(path string) error {
	if path == "" {
		return fmt.Errorf("shard path required")
	}
	for _, suf := range []string{".arena", ".arena.ids"} {
		if len(path) >= len(suf) && path[len(path)-len(suf):] == suf {
			return fmt.Errorf("refus arène/monolithe: chemin %q interdit", path)
		}
	}
	info, err := os.Stat(path)
	if err == nil && info.IsDir() {
		return fmt.Errorf("refus: shard %q est un répertoire", path)
	}
	return nil
}
