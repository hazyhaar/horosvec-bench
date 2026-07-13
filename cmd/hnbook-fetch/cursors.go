package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const (
	cursorHead     = "head"
	cursorBackfill = "backfill"
	// cursorHeadOrigin fige le maxitem observé à la PREMIÈRE init : c'est le plafond
	// immuable du rattrapage. La tête remplit (head_origin, maintenant], le rattrapage
	// remplit (28,7M, head_origin] — les deux zones ne se recouvrent JAMAIS, donc le
	// rattrapage ne peut re-ingérer un item déjà pris par la tête (convergence sûre).
	cursorHeadOrigin = "head_origin"
)

type cursorStore struct {
	db *sql.DB
}

func openCursorStore(path string) (*cursorStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	cs := &cursorStore{db: db}
	if err := cs.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return cs, nil
}

func (cs *cursorStore) Close() error {
	return cs.db.Close()
}

func (cs *cursorStore) initSchema(ctx context.Context) error {
	_, err := cs.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS cursor(
  name TEXT PRIMARY KEY,
  pos INTEGER NOT NULL
)`)
	if err != nil {
		return fmt.Errorf("cursor schema: %w", err)
	}
	_, err = cs.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS run_log(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  started_at TEXT NOT NULL,
  ended_at TEXT,
  head_ingested INT NOT NULL DEFAULT 0,
  backfill_ingested INT NOT NULL DEFAULT 0,
  head_skipped INT NOT NULL DEFAULT 0,
  backfill_skipped INT NOT NULL DEFAULT 0,
  head_pos INT,
  backfill_pos INT,
  status TEXT NOT NULL,
  error TEXT
)`)
	if err != nil {
		return fmt.Errorf("run_log schema: %w", err)
	}
	return nil
}

type runLogRow struct {
	ID               int64
	StartedAt        string
	EndedAt          sql.NullString
	HeadIngested     int
	BackfillIngested int
	HeadSkipped      int
	BackfillSkipped  int
	HeadPos          sql.NullInt64
	BackfillPos      sql.NullInt64
	Status           string
	Error            sql.NullString
}

func (cs *cursorStore) StartRun(ctx context.Context, startedAt time.Time) (int64, error) {
	res, err := cs.db.ExecContext(ctx,
		`INSERT INTO run_log(started_at, status) VALUES(?, 'running')`,
		startedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return 0, fmt.Errorf("run_log start: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("run_log id: %w", err)
	}
	return id, nil
}

func (cs *cursorStore) EndRun(
	ctx context.Context,
	runID int64,
	endedAt time.Time,
	stats runStats,
	headPos, backfillPos int64,
	errStr string,
) error {
	status := "ok"
	if errStr != "" {
		status = "error"
	}
	_, err := cs.db.ExecContext(ctx, `
UPDATE run_log SET
  ended_at = ?,
  head_ingested = ?,
  backfill_ingested = ?,
  head_skipped = ?,
  backfill_skipped = ?,
  head_pos = ?,
  backfill_pos = ?,
  status = ?,
  error = ?
WHERE id = ?`,
		endedAt.UTC().Format(time.RFC3339),
		stats.headIngested,
		stats.backfillIngested,
		stats.headSkipped,
		stats.backfillSkipped,
		headPos,
		backfillPos,
		status,
		nullIfEmpty(errStr),
		runID,
	)
	if err != nil {
		return fmt.Errorf("run_log end: %w", err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (cs *cursorStore) lastRuns(ctx context.Context, n int) ([]runLogRow, error) {
	if n < 1 {
		return nil, nil
	}
	rows, err := cs.db.QueryContext(ctx, `
SELECT id, started_at, ended_at,
       head_ingested, backfill_ingested, head_skipped, backfill_skipped,
       head_pos, backfill_pos, status, error
FROM run_log
ORDER BY id DESC
LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("run_log query: %w", err)
	}
	defer rows.Close()

	var out []runLogRow
	for rows.Next() {
		var r runLogRow
		if err := rows.Scan(
			&r.ID, &r.StartedAt, &r.EndedAt,
			&r.HeadIngested, &r.BackfillIngested, &r.HeadSkipped, &r.BackfillSkipped,
			&r.HeadPos, &r.BackfillPos, &r.Status, &r.Error,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (cs *cursorStore) read(ctx context.Context, name string) (int64, bool, error) {
	var pos int64
	err := cs.db.QueryRowContext(ctx, `SELECT pos FROM cursor WHERE name = ?`, name).Scan(&pos)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return pos, true, nil
}

func (cs *cursorStore) readBoth(ctx context.Context) (head, backfill int64, err error) {
	head, _, err = cs.read(ctx, cursorHead)
	if err != nil {
		return 0, 0, err
	}
	backfill, _, err = cs.read(ctx, cursorBackfill)
	if err != nil {
		return 0, 0, err
	}
	return head, backfill, nil
}

func (cs *cursorStore) initIfMissing(ctx context.Context, headStart, backfillStart int64) (head, backfill int64, err error) {
	tx, err := cs.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	head, headOK, err := readCursorTx(ctx, tx, cursorHead)
	if err != nil {
		return 0, 0, err
	}
	if !headOK {
		head = headStart
		if err := upsertCursorTx(ctx, tx, cursorHead, head); err != nil {
			return 0, 0, err
		}
	}

	backfill, backOK, err := readCursorTx(ctx, tx, cursorBackfill)
	if err != nil {
		return 0, 0, err
	}
	if !backOK {
		backfill = backfillStart
		if err := upsertCursorTx(ctx, tx, cursorBackfill, backfill); err != nil {
			return 0, 0, err
		}
	}

	// head_origin figé à la première init (= maxitem du jour d'amorçage) ; plafond immuable
	// du rattrapage. Jamais réécrit ensuite : c'est la frontière fixe entre les deux zones.
	if _, originOK, err := readCursorTx(ctx, tx, cursorHeadOrigin); err != nil {
		return 0, 0, err
	} else if !originOK {
		if err := upsertCursorTx(ctx, tx, cursorHeadOrigin, headStart); err != nil {
			return 0, 0, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return head, backfill, nil
}

func readCursorTx(ctx context.Context, tx *sql.Tx, name string) (int64, bool, error) {
	var pos int64
	err := tx.QueryRowContext(ctx, `SELECT pos FROM cursor WHERE name = ?`, name).Scan(&pos)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return pos, true, nil
}

func upsertCursorTx(ctx context.Context, tx *sql.Tx, name string, pos int64) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO cursor(name, pos) VALUES(?, ?) ON CONFLICT(name) DO UPDATE SET pos = excluded.pos`,
		name, pos,
	)
	return err
}

// advance commit la nouvelle position du curseur nommé après ingestion réussie ou skip.
func (cs *cursorStore) advance(ctx context.Context, name string, pos int64) error {
	tx, err := cs.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := upsertCursorTx(ctx, tx, name, pos); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
