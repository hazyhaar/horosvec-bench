// Command hnbook-embed lit un flux NDJSON (id + texte concaténé titre/corps, produit par
// duckdb en amont depuis le Parquet HackerNews), embedde le corpus via un endpoint
// OpenAI-compatible (qwen3-embedding-0.6b en MRL 512) et écrit une arène plate fp16
// (format HVARENA1 de horosvec) + un fichier d'ids HN + un manifest checkpointé, au fil de
// l'eau. Reprise idempotente au checkpoint après interruption : ni doublon ni trou.
//
// Écrivain d'arène UNIQUE et séquentiel (le collecteur) ; le parallélisme porte sur les POST
// d'embedding (concurrence bornée), avec ré-ordonnancement par rang avant écriture —
// l'offset dans l'arène est le rang de l'item, l'ordre du flux fait foi.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "hnbook-embed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		inPath       = flag.String("in", "-", "flux NDJSON d'entrée (id,text) ; '-' = stdin")
		arenaPath    = flag.String("arena", "", "chemin de l'arène fp16 de sortie (HVARENA1)")
		idsPath      = flag.String("ids", "", "chemin du fichier d'ids HN (uint64 LE, défaut <arena>.ids)")
		manifestPath = flag.String("manifest", "", "chemin du manifest checkpoint (défaut <arena>.manifest.json)")
		endpoint     = flag.String("endpoint", "http://127.0.0.1:8001/v1/embeddings", "endpoint OpenAI /v1/embeddings")
		model        = flag.String("model", "qwen3-embedding-0.6b", "modèle d'embedding")
		dims         = flag.Int("dims", 512, "dimensions MRL demandées au serveur (0 = natif)")
		batch        = flag.Int("batch", 128, "taille de lot")
		concurrency  = flag.Int("concurrency", 8, "POST concurrents")
		checkpoint   = flag.Int("checkpoint-batches", 1, "nombre de lots entre deux checkpoints durables")
		progress     = flag.Duration("progress", 5*time.Second, "intervalle de log de progression")
	)
	flag.Parse()

	if *arenaPath == "" {
		return fmt.Errorf("-arena est obligatoire")
	}
	idsP := *idsPath
	if idsP == "" {
		idsP = *arenaPath + ".ids"
	}
	manifestP := *manifestPath
	if manifestP == "" {
		manifestP = *arenaPath + ".manifest.json"
	}

	in := os.Stdin
	if *inPath != "-" {
		f, err := os.Open(*inPath)
		if err != nil {
			return fmt.Errorf("ouverture -in: %w", err)
		}
		defer f.Close()
		in = f
	}

	// SIGINT/SIGTERM annulent proprement le contexte : les checkpoints déjà écrits restent
	// valides, le manifest permet la reprise. Aucune arène finale n'est produite tant que
	// finalize n'a pas tourné (le .tmp est repris au run suivant).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := pipelineConfig{
		arenaPath:        *arenaPath,
		idsPath:          idsP,
		manifestPath:     manifestP,
		endpoint:         *endpoint,
		model:            *model,
		dims:             *dims,
		batchSize:        *batch,
		concurrency:      *concurrency,
		checkpointEvery:  *checkpoint,
		progressInterval: *progress,
	}
	return runPipeline(ctx, cfg, in)
}
