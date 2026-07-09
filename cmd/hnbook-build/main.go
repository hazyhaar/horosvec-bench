// hnbook-build construit un index horosvec depuis une arène fp16 EXISTANTE
// (produite par hnbook-embed) via BuildFromArena — le runner de la vague V2 du
// méta-goal « mise en index HNbook » (Job 019f4058), utilisé aussi pour la sonde
// de scaling sur préfixe d'arène. Aucune matérialisation fp32 pleine : les
// vecteurs sont lus à la demande depuis l'arène mmap.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/hazyhaar/horosvec"
	_ "modernc.org/sqlite"
)

func main() {
	arenaPath := flag.String("arena", "", "arène fp16 HVARENA1 (complète, manifest done)")
	idsPath := flag.String("ids", "", "fichier d'ids (uint64 LE, rang = node_id ; défaut <arena>.ids)")
	outPath := flag.String("out", "", "index SQLite de sortie (créé/écrasé)")
	workers := flag.Int("workers", 0, "BuildWorkers (0 = défaut horosvec, ~40% des cœurs)")
	flag.Parse()
	if *arenaPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "usage: hnbook-build -arena <path> -out <index.db> [-ids <path>] [-workers N]")
		os.Exit(2)
	}
	if *idsPath == "" {
		*idsPath = *arenaPath + ".ids"
	}

	db, err := sql.Open("sqlite", *outPath)
	if err != nil {
		fatal("open sqlite: %v", err)
	}
	defer db.Close()

	cfg := horosvec.DefaultConfig()
	cfg.ArenaPath = *arenaPath
	if *workers > 0 {
		cfg.BuildWorkers = *workers
	}
	idx, err := horosvec.New(db, cfg)
	if err != nil {
		fatal("horosvec new: %v", err)
	}

	slog.Info("hnbook-build: démarrage", "arena", *arenaPath, "ids", *idsPath, "out", *outPath)
	t0 := time.Now()
	if err := idx.BuildFromArena(context.Background(), *arenaPath, *idsPath); err != nil {
		fatal("build from arena: %v", err)
	}
	slog.Info("hnbook-build: terminé", "build_s", time.Since(t0).Seconds())
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "hnbook-build: "+format+"\n", args...)
	os.Exit(1)
}
