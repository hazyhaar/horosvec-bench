package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
)

func main() {
	var (
		n    int
		dim  int
		seed uint64
		out  string
	)
	flag.IntVar(&n, "n", 2000, "nombre de vecteurs")
	flag.IntVar(&dim, "dim", 64, "dimension")
	flag.Uint64Var(&seed, "seed", 42, "graine aléatoire")
	flag.StringVar(&out, "out", "", "fichier JSONL de sortie")
	flag.Parse()

	if out == "" {
		fmt.Fprintln(os.Stderr, "gen-random: -out requis")
		os.Exit(1)
	}

	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-random: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	enc := json.NewEncoder(f)
	for i := 0; i < n; i++ {
		vec := make([]float32, dim)
		for j := range vec {
			vec[j] = rng.Float32()
		}
		if err := enc.Encode(vec); err != nil {
			fmt.Fprintf(os.Stderr, "gen-random: %v\n", err)
			os.Exit(1)
		}
	}
}
