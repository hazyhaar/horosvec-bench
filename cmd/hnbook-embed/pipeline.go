package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/hazyhaar/horosvec"
)

// pipelineConfig porte les paramètres du run.
type pipelineConfig struct {
	arenaPath        string
	idsPath          string
	manifestPath     string
	endpoint         string
	model            string
	dims             int // dimension MRL demandée (512) ; sert aussi de dim d'arène si >0
	batchSize        int // 128
	concurrency      int // 8
	checkpointEvery  int // nombre de lots entre deux checkpoints durables (>=1)
	progressInterval time.Duration
}

// batch : une tranche contiguë d'items (offset = startRank + i).
type batch struct {
	startRank int64
	ids       []uint64
	texts     []string
}

// result : les vecteurs d'un lot, à réordonner par startRank avant écriture.
type result struct {
	startRank int64
	ids       []uint64
	vecs      [][]float32
}

// ndjsonLine est la forme attendue d'une ligne du flux (produit par duckdb en amont) :
// l'id HN natif et le texte concaténé titre+corps.
type ndjsonLine struct {
	ID   json.Number `json:"id"`
	Text string      `json:"text"`
}

// runPipeline exécute le pipeline complet : lecture NDJSON, embedding concurrent, écriture
// d'arène + ids checkpointée, reprise au manifest. Écrivain d'arène UNIQUE (le collecteur),
// parallélisme borné sur les POST, ré-ordonnancement par rang avant écriture.
func runPipeline(ctx context.Context, cfg pipelineConfig, in io.Reader) error {
	if cfg.batchSize <= 0 || cfg.concurrency <= 0 || cfg.checkpointEvery <= 0 {
		return fmt.Errorf("hnbook-embed: paramètres invalides (batch=%d conc=%d ckpt=%d)", cfg.batchSize, cfg.concurrency, cfg.checkpointEvery)
	}

	// Reprise : lire le manifest existant.
	prev, err := readManifest(cfg.manifestPath)
	if err != nil {
		return err
	}
	var resumeCount int64
	dim := cfg.dims
	if prev != nil {
		if prev.Status == statusDone {
			slog.Info("hnbook-embed: run déjà terminé selon le manifest", "count", prev.Count)
			return nil
		}
		// Finalize interrompu : si l'arène finale existe déjà (le rename atomique a eu lieu),
		// son .tmp a disparu et une reprise ordinaire échouerait à le rouvrir. On complète la
		// finalisation de façon idempotente (fenêtre entre le rename de l'arène et le passage
		// du manifest à "done"). Sans données manquantes : le checkpoint de finalize a déjà
		// durci le count complet.
		if _, serr := os.Stat(cfg.arenaPath); serr == nil {
			return completeInterruptedFinalize(cfg, *prev)
		}
		resumeCount = prev.Count
		dim = prev.Dim
		if cfg.dims > 0 && dim != cfg.dims {
			return fmt.Errorf("hnbook-embed: dim manifest %d != -dims %d", dim, cfg.dims)
		}
		slog.Info("hnbook-embed: reprise", "checkpoint_count", resumeCount, "dim", dim)
	}

	col := &collector{
		cfg:      cfg,
		nextRank: resumeCount,
		pending:  make(map[int64]result),
		started:  time.Now(),
		dim:      dim,
	}
	if resumeCount > 0 {
		if err := col.resumeWriters(); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		errOnce  sync.Once
		firstErr error
	)
	fail := func(e error) {
		errOnce.Do(func() {
			firstErr = e
			cancel()
		})
	}

	batchCh := make(chan batch, cfg.concurrency*2)
	resultCh := make(chan result, cfg.concurrency*2)

	// Producteur : lit le NDJSON, saute les resumeCount premiers items, forme les lots.
	go func() {
		// defer/recover : un panic est routé vers fail plutôt que de crasher le process.
		defer func() {
			if r := recover(); r != nil {
				fail(fmt.Errorf("hnbook-embed: panic producteur: %v", r))
			}
		}()
		if err := produce(ctx, in, cfg.batchSize, resumeCount, batchCh); err != nil {
			fail(err)
		}
		close(batchCh)
	}()

	// Workers : POST concurrents.
	// dim résolue (manifest sur reprise, sinon -dims) : le client doit demander au serveur la
	// MÊME dimension MRL que le checkpoint, sinon les vecteurs repris auraient une autre dim.
	client := newEmbedClient(cfg.endpoint, cfg.model, dim)
	var wg sync.WaitGroup
	for i := 0; i < cfg.concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					fail(fmt.Errorf("hnbook-embed: panic worker: %v", r))
				}
			}()
			for b := range batchCh {
				vecs, err := client.embedBatch(ctx, b.texts)
				if err != nil {
					fail(err)
					return
				}
				select {
				case resultCh <- result{startRank: b.startRank, ids: b.ids, vecs: vecs}:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fail(fmt.Errorf("hnbook-embed: panic closer: %v", r))
			}
		}()
		wg.Wait()
		close(resultCh)
	}()

	// Collecteur (écrivain unique) : réordonne par rang et écrit arène + ids, checkpointé.
	ticker := time.NewTicker(cfg.progressInterval)
	defer ticker.Stop()
	for {
		select {
		case r, ok := <-resultCh:
			if !ok {
				if firstErr != nil {
					// Erreur dure : le .tmp + le dernier manifest restent pour reprise
					// ou inspection ; aucune arène finale n'est produite.
					return firstErr
				}
				if cerr := ctx.Err(); cerr != nil {
					// Interruption (SIGINT/SIGTERM) : durcir les lots entiers déjà écrits
					// puis sortir SANS finaliser. Le run suivant reprend au manifest.
					if err := col.checkpoint(); err != nil {
						return err
					}
					return cerr
				}
				return col.finalize()
			}
			if ctx.Err() != nil {
				// fail() appelle toujours cancel() ; ctx.Err() est sûr en concurrence (la
				// lecture nue de firstErr le serait pas — course avec l'écriture sous
				// errOnce.Do d'un worker). On draine sans traiter, les workers sortent sur
				// ctx.Done().
				continue
			}
			if err := col.ingest(r); err != nil {
				fail(err)
			}
		case <-ticker.C:
			col.logProgress()
		}
	}
}

// completeInterruptedFinalize termine idempotemment une finalisation interrompue : l'arène
// finale existe (rename effectué) mais le manifest est resté "in_progress". Il ne reste qu'à
// s'assurer que l'ids final est en place (son rename a pu ne pas avoir eu lieu) et à basculer
// le manifest à "done", après vérification de cohérence des comptes. Aucune donnée n'est
// re-embeddée.
func completeInterruptedFinalize(cfg pipelineConfig, m Manifest) error {
	ar, err := horosvec.OpenArenaReader(cfg.arenaPath)
	if err != nil {
		return fmt.Errorf("hnbook-embed: finalize interrompu, arène illisible: %w", err)
	}
	if ar.Count() != m.Count {
		return fmt.Errorf("hnbook-embed: finalize interrompu, arène count %d != manifest %d", ar.Count(), m.Count)
	}
	if _, serr := os.Stat(cfg.idsPath); serr != nil {
		if !os.IsNotExist(serr) {
			return fmt.Errorf("hnbook-embed: finalize interrompu, stat ids: %w", serr)
		}
		// Le rename ids.tmp→ids n'avait pas encore eu lieu ; l'ids.tmp a été durci par le
		// checkpoint de finalize.
		if err := os.Rename(cfg.idsPath+".tmp", cfg.idsPath); err != nil {
			return fmt.Errorf("hnbook-embed: finalize interrompu, rename ids: %w", err)
		}
	}
	ids, err := readIDs(cfg.idsPath)
	if err != nil {
		return err
	}
	if int64(len(ids)) != m.Count {
		return fmt.Errorf("hnbook-embed: finalize interrompu, ids %d != manifest %d", len(ids), m.Count)
	}
	m.Status = statusDone
	if err := writeManifest(cfg.manifestPath, m); err != nil {
		return err
	}
	slog.Info("hnbook-embed: finalize interrompu complété", "count", m.Count)
	return nil
}

// produce lit le flux NDJSON et émet des lots contigus, en sautant skip items déjà traités.
func produce(ctx context.Context, in io.Reader, batchSize int, skip int64, out chan<- batch) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // textes HN jusqu'à ~6000 car., marge large
	var rank int64
	cur := batch{startRank: skip}
	flush := func() error {
		if len(cur.texts) == 0 {
			return nil
		}
		select {
		case out <- cur:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if rank < skip {
			rank++
			continue
		}
		var nl ndjsonLine
		if err := json.Unmarshal(line, &nl); err != nil {
			return fmt.Errorf("hnbook-embed: parse ligne rang %d: %w", rank, err)
		}
		id, err := strconv.ParseUint(string(nl.ID), 10, 64)
		if err != nil {
			return fmt.Errorf("hnbook-embed: id non entier rang %d (%q): %w", rank, nl.ID.String(), err)
		}
		// Texte HTML échappé conservé tel quel : le modèle d'embedding s'en accommode et
		// l'éligibilité (non vide) est garantie en amont par la requête duckdb. Un texte vide
		// ici est une violation de contrat amont → fail-loud.
		if nl.Text == "" {
			return fmt.Errorf("hnbook-embed: texte vide rang %d (id %d) — contrat amont violé", rank, id)
		}
		cur.ids = append(cur.ids, id)
		cur.texts = append(cur.texts, nl.Text)
		rank++
		if len(cur.texts) == batchSize {
			if err := flush(); err != nil {
				return err
			}
			cur = batch{startRank: rank}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("hnbook-embed: lecture NDJSON: %w", err)
	}
	return flush()
}

// collector est l'écrivain unique d'arène + ids, avec réordonnancement et checkpoint.
type collector struct {
	cfg      pipelineConfig
	arena    *horosvec.ArenaWriter
	ids      *idsWriter
	dim      int
	nextRank int64
	pending  map[int64]result

	batchesSinceCkpt int
	started          time.Time
	writtenThisRun   int64
}

func (c *collector) resumeWriters() error {
	if c.dim <= 0 {
		return fmt.Errorf("hnbook-embed: reprise sans dimension connue")
	}
	aw, err := horosvec.ResumeArenaWriter(c.cfg.arenaPath, c.dim, c.nextRank)
	if err != nil {
		return err
	}
	iw, err := resumeIDsWriter(c.cfg.idsPath, c.nextRank)
	if err != nil {
		aw.Abort()
		return err
	}
	c.arena = aw
	c.ids = iw
	return nil
}

func (c *collector) createWriters(dim int) error {
	c.dim = dim
	aw, err := horosvec.NewArenaWriter(c.cfg.arenaPath, dim)
	if err != nil {
		return err
	}
	iw, err := newIDsWriter(c.cfg.idsPath)
	if err != nil {
		aw.Abort()
		return err
	}
	c.arena = aw
	c.ids = iw
	return nil
}

// ingest range un résultat puis écrit tous les lots devenus contigus au rang courant.
func (c *collector) ingest(r result) error {
	c.pending[r.startRank] = r
	for {
		r, ok := c.pending[c.nextRank]
		if !ok {
			return nil
		}
		if err := c.writeBatch(r); err != nil {
			return err
		}
		delete(c.pending, c.nextRank)
		c.nextRank += int64(len(r.vecs))
		c.batchesSinceCkpt++
		if c.batchesSinceCkpt >= c.cfg.checkpointEvery {
			if err := c.checkpoint(); err != nil {
				return err
			}
		}
	}
}

func (c *collector) writeBatch(r result) error {
	if c.arena == nil {
		// Création paresseuse : dimension déduite du premier vecteur (si -dims=0).
		dim := c.dim
		if dim <= 0 {
			dim = len(r.vecs[0])
		}
		if err := c.createWriters(dim); err != nil {
			return err
		}
	}
	for i, v := range r.vecs {
		if len(v) != c.dim {
			return fmt.Errorf("hnbook-embed: vecteur dim %d != %d (rang %d)", len(v), c.dim, r.startRank+int64(i))
		}
		if err := c.arena.WriteVec(v); err != nil {
			return err
		}
		if err := c.ids.write(r.ids[i]); err != nil {
			return err
		}
		c.writtenThisRun++
	}
	return nil
}

// checkpoint durcit arène + ids (fsync) PUIS écrit le manifest. Ordre impératif : le count
// du manifest ne doit jamais dépasser les octets durables (sinon la reprise créerait un
// trou). Un crash entre les fsync et le manifest laisse le manifest en retard → la reprise
// tronque et refait la tranche (idempotent, ni doublon ni trou).
func (c *collector) checkpoint() error {
	if c.arena == nil {
		return nil
	}
	if err := c.arena.Sync(); err != nil {
		return err
	}
	if err := c.ids.sync(); err != nil {
		return err
	}
	if err := writeManifest(c.cfg.manifestPath, Manifest{
		Magic:     manifestMagic,
		Model:     c.cfg.model,
		Dim:       c.dim,
		ArenaPath: c.cfg.arenaPath,
		IDsPath:   c.cfg.idsPath,
		Count:     c.nextRank,
		Status:    statusInProgress,
	}); err != nil {
		return err
	}
	c.batchesSinceCkpt = 0
	return nil
}

// finalize checkpoint une dernière fois, finalise arène + ids (rename atomique) et marque le
// manifest done.
func (c *collector) finalize() error {
	if c.arena == nil {
		// Aucun item écrit (flux vide et pas de reprise).
		if c.nextRank == 0 {
			return fmt.Errorf("hnbook-embed: aucun vecteur produit (flux vide)")
		}
		return nil
	}
	if err := c.checkpoint(); err != nil {
		return err
	}
	if err := c.arena.Finalize(); err != nil {
		return err
	}
	c.arena = nil
	if err := c.ids.finalize(); err != nil {
		return err
	}
	c.ids = nil
	if err := writeManifest(c.cfg.manifestPath, Manifest{
		Magic:     manifestMagic,
		Model:     c.cfg.model,
		Dim:       c.dim,
		ArenaPath: c.cfg.arenaPath,
		IDsPath:   c.cfg.idsPath,
		Count:     c.nextRank,
		Status:    statusDone,
	}); err != nil {
		return err
	}
	slog.Info("hnbook-embed: terminé", "count", c.nextRank, "dim", c.dim,
		"docs_per_s", c.docsPerSec())
	return nil
}

func (c *collector) docsPerSec() float64 {
	el := time.Since(c.started).Seconds()
	if el <= 0 {
		return 0
	}
	return float64(c.writtenThisRun) / el
}

func (c *collector) logProgress() {
	slog.Info("hnbook-embed: progression",
		"count", c.nextRank,
		"written_this_run", c.writtenThisRun,
		"docs_per_s", fmt.Sprintf("%.1f", c.docsPerSec()),
		"pending_batches", len(c.pending))
}
