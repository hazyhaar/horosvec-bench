// jsonl2arena convertit un préfixe d'un fichier jsonl de vecteurs réels (une ligne =
// un tableau JSON de float) en arène fp16 HVARENA1 + fichier d'ids jumeau, en streaming
// (une ligne lue, un vecteur écrit), sans jamais matérialiser le jeu complet en RAM.
// Outil jetable de mesure (proj_horosvec_chrono_build_cpu) : produit l'entrée de
// hnbook-build (BuildFromArena, constructeur Vamana pur-Go) à des échelles croissantes
// sur le même corpus réel dim512 que le reste de horosvec-bench.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/hazyhaar/horosvec"
)

func main() {
	in := flag.String("in", "", "jsonl source (une ligne = un tableau JSON de float)")
	out := flag.String("out", "", "arène fp16 de sortie (.arena)")
	limit := flag.Int("limit", 0, "nombre de vecteurs à écrire (préfixe, 0 = tout)")
	flag.Parse()
	if *in == "" || *out == "" || *limit <= 0 {
		fmt.Fprintln(os.Stderr, "usage: jsonl2arena -in <base.jsonl> -out <prefix.arena> -limit N")
		os.Exit(2)
	}

	t0 := time.Now()
	f, err := os.Open(*in)
	if err != nil {
		fatal("open: %v", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)

	var w *horosvec.ArenaWriter
	idsPath := *out + ".ids"
	idsF, err := os.Create(idsPath)
	if err != nil {
		fatal("create ids: %v", err)
	}
	idsW := bufio.NewWriter(idsF)

	n := 0
	dim := 0
	for sc.Scan() && n < *limit {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw []float64
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			fatal("ligne %d: %v", n+1, err)
		}
		if w == nil {
			dim = len(raw)
			w, err = horosvec.NewArenaWriter(*out, dim)
			if err != nil {
				fatal("new arena writer: %v", err)
			}
		}
		if len(raw) != dim {
			fatal("ligne %d: dim %d != %d", n+1, len(raw), dim)
		}
		vec := make([]float32, dim)
		for i, v := range raw {
			vec[i] = float32(v)
		}
		if err := w.WriteVec(vec); err != nil {
			w.Abort()
			fatal("write vec %d: %v", n, err)
		}
		if err := writeIDLE(idsW, uint64(n)); err != nil {
			w.Abort()
			fatal("write id %d: %v", n, err)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		if w != nil {
			w.Abort()
		}
		fatal("scan: %v", err)
	}
	if n == 0 {
		fatal("aucun vecteur lu (source vide ou limit=0)")
	}
	if err := w.Finalize(); err != nil {
		fatal("finalize: %v", err)
	}
	if err := idsW.Flush(); err != nil {
		fatal("flush ids: %v", err)
	}
	if err := idsF.Close(); err != nil {
		fatal("close ids: %v", err)
	}

	slog.Info("jsonl2arena: terminé", "n", n, "dim", dim, "out", *out, "ids", idsPath, "elapsed_s", time.Since(t0).Seconds())
}

func writeIDLE(w *bufio.Writer, id uint64) error {
	var buf [8]byte
	for i := 0; i < 8; i++ {
		buf[i] = byte(id >> (8 * uint(i)))
	}
	_, err := w.Write(buf[:])
	return err
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "jsonl2arena: "+format+"\n", args...)
	os.Exit(1)
}
