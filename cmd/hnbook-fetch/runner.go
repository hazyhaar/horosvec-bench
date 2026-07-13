package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type backfillBudget struct {
	maxItems int
	maxDur   time.Duration
}

func parseBackfillBudget(s string) (backfillBudget, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return backfillBudget{}, fmt.Errorf("backfill-budget vide")
	}
	if strings.HasSuffix(s, "s") {
		d, err := time.ParseDuration(s)
		if err != nil {
			return backfillBudget{}, fmt.Errorf("backfill-budget durée: %w", err)
		}
		return backfillBudget{maxDur: d}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		// Un budget de rattrapage nul serait interprété comme "illimité" par la garde
		// budget.maxItems>0 du moteur (anti-crash : un run non borné doit être impossible).
		// Pour ne PAS faire de rattrapage, ne pas lancer le mode rattrapage — jamais budget 0.
		return backfillBudget{}, fmt.Errorf("backfill-budget items doit être > 0 (ou une durée Ns) ; %q rendrait le rattrapage non borné", s)
	}
	return backfillBudget{maxItems: n}, nil
}

type runConfig struct {
	cursorsPath    string
	storePath      string
	shardPath      string
	titlesBin      string
	embedURL       string
	hnBaseURL      string
	backfillStart  int64
	concurrency    int
	backfillBudget backfillBudget
	maxHead        int
	once           bool
}

type runStats struct {
	headIngested     int
	backfillIngested int
	headSkipped      int
	backfillSkipped  int
}

type runner struct {
	cfg     runConfig
	cursors *cursorStore
	hn      *hnClient
	ing     ingestor
}

func newRunner(cfg runConfig, hc, ec *http.Client) (*runner, error) {
	if err := rejectArenaShardPath(cfg.shardPath); err != nil {
		return nil, err
	}
	cs, err := openCursorStore(cfg.cursorsPath)
	if err != nil {
		return nil, err
	}
	shard, err := openShardIndex(cfg.shardPath)
	if err != nil {
		_ = cs.Close()
		return nil, err
	}
	maxItem, err := newHNClient(cfg.hnBaseURL, hc, cfg.concurrency).MaxItem(context.Background())
	if err != nil {
		_ = shard.Close()
		_ = cs.Close()
		return nil, err
	}
	head, backfill, err := cs.initIfMissing(context.Background(), maxItem, cfg.backfillStart)
	if err != nil {
		_ = shard.Close()
		_ = cs.Close()
		return nil, err
	}
	slog.Info("curseurs init", "head", head, "backfill", backfill, "maxitem", maxItem)

	return &runner{
		cfg:     cfg,
		cursors: cs,
		hn:      newHNClient(cfg.hnBaseURL, hc, cfg.concurrency),
		ing: ingestor{
			titles: titlesAppender{bin: cfg.titlesBin},
			embed:  newEmbedClient(cfg.embedURL, ec),
			shard:  shard,
			store:  cfg.storePath,
		},
	}, nil
}

func (r *runner) Close() error {
	if r.ing.shard != nil {
		_ = r.ing.shard.Close()
	}
	if r.cursors != nil {
		return r.cursors.Close()
	}
	return nil
}

func (r *runner) runCycle(ctx context.Context) (runStats, error) {
	startedAt := time.Now()
	runID, err := r.cursors.StartRun(ctx, startedAt)
	if err != nil {
		return runStats{}, err
	}

	var stats runStats
	var cycleErr error
	headPos, backfillPos := int64(0), int64(0)

	defer func() {
		errStr := ""
		if cycleErr != nil {
			errStr = cycleErr.Error()
		}
		if endErr := r.cursors.EndRun(ctx, runID, time.Now(), stats, headPos, backfillPos, errStr); endErr != nil {
			slog.Error("run_log end", "err", endErr)
		}
	}()

	maxItem, err := r.hn.MaxItem(ctx)
	if err != nil {
		cycleErr = err
		return stats, cycleErr
	}
	head, backfill, err := r.cursors.readBoth(ctx)
	if err != nil {
		cycleErr = err
		return stats, cycleErr
	}
	headPos, backfillPos = head, backfill

	seen := make(map[int64]struct{})

	// Plafond IMMUABLE du rattrapage : le maxitem figé à la première init. Le rattrapage ne
	// franchit jamais cette frontière, donc ne peut re-ingérer un item de la zone de tête
	// (convergence sûre — cf. cursorHeadOrigin). Repli sur head pour d'anciens fichiers de
	// curseurs sans la ligne head_origin.
	headOrigin, originOK, err := r.cursors.read(ctx, cursorHeadOrigin)
	if err != nil {
		cycleErr = err
		return stats, cycleErr
	}
	if !originOK {
		headOrigin = head
	}

	if maxItem > head {
		headBudget := backfillBudget{}
		if r.cfg.maxHead > 0 {
			headBudget.maxItems = r.cfg.maxHead
		}
		slog.Info("phase tête", "from", head+1, "to", maxItem, "max_head", r.cfg.maxHead)
		h, ing, sk, err := r.processRange(ctx, cursorHead, "head", head, maxItem, 0, seen, headBudget)
		headPos = h
		if err != nil {
			cycleErr = err
			return stats, cycleErr
		}
		head = h
		stats.headIngested = ing
		stats.headSkipped = sk
	}

	if backfill < headOrigin {
		slog.Info("phase rattrapage", "from", backfill+1, "head_origin", headOrigin, "budget", r.cfg.backfillBudget)
		b, ing, sk, err := r.processRange(ctx, cursorBackfill, "backfill", backfill, headOrigin, headOrigin+1, seen, r.cfg.backfillBudget)
		backfillPos = b
		if err != nil {
			cycleErr = err
			return stats, cycleErr
		}
		backfill = b
		stats.backfillIngested = ing
		stats.backfillSkipped = sk
	}

	slog.Info("cycle terminé",
		"head", head, "backfill", backfill,
		"head_ingested", stats.headIngested, "backfill_ingested", stats.backfillIngested)
	return stats, nil
}

func (r *runner) processRange(
	ctx context.Context,
	cursorName, phase string,
	pos, end, headFloor int64,
	seen map[int64]struct{},
	budget backfillBudget,
) (newPos int64, ingested, skipped int, err error) {
	newPos = pos
	if end < pos+1 {
		return newPos, 0, 0, nil
	}
	deadline := time.Time{}
	if budget.maxDur > 0 {
		deadline = time.Now().Add(budget.maxDur)
	}
	processed := 0

	for id := pos + 1; id <= end; id++ {
		if ctx.Err() != nil {
			return newPos, ingested, skipped, ctx.Err()
		}
		if budget.maxItems > 0 && processed >= budget.maxItems {
			break
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			break
		}
		if headFloor > 0 && id >= headFloor {
			break
		}
		if _, dup := seen[id]; dup {
			if err := r.cursors.advance(ctx, cursorName, id); err != nil {
				return newPos, ingested, skipped, err
			}
			newPos = id
			skipped++
			processed++
			continue
		}
		seen[id] = struct{}{}

		item, err := r.hn.FetchItem(ctx, id)
		if err != nil {
			return newPos, ingested, skipped, fmt.Errorf("%s id=%d fetch: %w", phase, id, err)
		}
		if item.skip() {
			if err := r.cursors.advance(ctx, cursorName, id); err != nil {
				return newPos, ingested, skipped, err
			}
			newPos = id
			skipped++
			processed++
			continue
		}

		ok, err := r.ing.ingestItem(ctx, item)
		if err != nil {
			return newPos, ingested, skipped, fmt.Errorf("%s id=%d %w", phase, id, err)
		}
		if err := r.cursors.advance(ctx, cursorName, id); err != nil {
			return newPos, ingested, skipped, err
		}
		newPos = id
		if ok {
			ingested++
		} else {
			skipped++
		}
		processed++
	}
	return newPos, ingested, skipped, nil
}

func (r *runner) shardNodeCount(ctx context.Context) (int64, error) {
	return r.ing.shard.nodeCount(ctx)
}
