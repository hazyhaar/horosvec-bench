// hnbook-fetch ingère le delta HackerNews via deux curseurs (tête + rattrapage)
// vers le magasin hnbook-titles et un shard horosvec db-blob distinct.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	var (
		cursorsPath   = flag.String("cursors", "cursors.db", "SQLite des curseurs head/backfill")
		storePath     = flag.String("store", "", "magasin hnbook-titles SQLite")
		shardPath     = flag.String("shard", "", "shard horosvec db-blob courant")
		titlesBin     = flag.String("titles-bin", "./bin/hnbook-titles", "binaire hnbook-titles")
		embedURL      = flag.String("embed-url", defaultEmbedURL, "URL sidecar embedding")
		hnBaseURL     = flag.String("hn-base", defaultHNBaseURL, "base API Hacker News")
		backfillStart = flag.Int64("backfill-start", defaultBackfillStart, "position initiale curseur rattrapage")
		concurrency   = flag.Int("concurrency", 8, "concurrence max fetch HN")
		backfillSpec  = flag.String("backfill-budget", "100", "budget rattrapage: N items ou Ts (ex. 30s)")
		maxHead       = flag.Int("max-head", 0, "borne items tête par run (0 = illimité)")
		logFile       = flag.String("log-file", "", "fichier log JSON slog en plus de stderr")
		showRuns      = flag.Int("show-runs", 0, "affiche les N derniers runs puis sort (0 = désactivé)")
		once          = flag.Bool("once", false, "un seul cycle puis sortie")
	)
	flag.Parse()

	if err := setupLogging(*logFile); err != nil {
		fmt.Fprintf(os.Stderr, "log-file: %v\n", err)
		os.Exit(2)
	}

	if *showRuns > 0 {
		if err := printLastRuns(context.Background(), *cursorsPath, *showRuns); err != nil {
			fatal("%v", err)
		}
		return
	}

	if *storePath == "" || *shardPath == "" {
		fmt.Fprintln(os.Stderr, "usage: hnbook-fetch -store <titles.db> -shard <shard.db> [options]")
		os.Exit(2)
	}

	budget, err := parseBackfillBudget(*backfillSpec)
	if err != nil {
		fatal("%v", err)
	}

	cfg := runConfig{
		cursorsPath:    *cursorsPath,
		storePath:      *storePath,
		shardPath:      *shardPath,
		titlesBin:      *titlesBin,
		embedURL:       *embedURL,
		hnBaseURL:      *hnBaseURL,
		backfillStart:  *backfillStart,
		concurrency:    *concurrency,
		backfillBudget: budget,
		maxHead:        *maxHead,
		once:           *once,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	run, err := newRunner(cfg, http.DefaultClient, http.DefaultClient)
	if err != nil {
		fatal("%v", err)
	}
	defer run.Close()

	for {
		if _, err := run.runCycle(ctx); err != nil {
			fatal("%v", err)
		}
		if cfg.once || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

func setupLogging(logFile string) error {
	writers := []io.Writer{os.Stderr}
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		writers = append(writers, f)
	}
	handler := slog.NewJSONHandler(io.MultiWriter(writers...), &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})
	slog.SetDefault(slog.New(handler))
	return nil
}

func printLastRuns(ctx context.Context, cursorsPath string, n int) error {
	cs, err := openCursorStore(cursorsPath)
	if err != nil {
		return err
	}
	defer cs.Close()

	rows, err := cs.lastRuns(ctx, n)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("aucun run enregistré")
		return nil
	}
	for _, r := range rows {
		ended := "-"
		if r.EndedAt.Valid {
			ended = r.EndedAt.String
		}
		errMsg := ""
		if r.Error.Valid {
			errMsg = r.Error.String
		}
		headPos := "-"
		if r.HeadPos.Valid {
			headPos = fmt.Sprintf("%d", r.HeadPos.Int64)
		}
		backfillPos := "-"
		if r.BackfillPos.Valid {
			backfillPos = fmt.Sprintf("%d", r.BackfillPos.Int64)
		}
		fmt.Printf("run #%d  started=%s  ended=%s  status=%s\n",
			r.ID, r.StartedAt, ended, r.Status)
		fmt.Printf("  head: ingested=%d skipped=%d pos=%s\n",
			r.HeadIngested, r.HeadSkipped, headPos)
		fmt.Printf("  backfill: ingested=%d skipped=%d pos=%s\n",
			r.BackfillIngested, r.BackfillSkipped, backfillPos)
		if errMsg != "" {
			fmt.Printf("  error: %s\n", errMsg)
		}
	}
	return nil
}

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
