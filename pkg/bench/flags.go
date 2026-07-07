package bench

import (
	"flag"
	"fmt"
)

// CommonFlags regroupe les flags partagés par les trois binaires.
type CommonFlags struct {
	Base    string
	Queries string
	GT      string
	K       int
	Limit   int
	Sweep   string
	Holdout int
}

// ParseFlags lit les flags CLI communs.
func ParseFlags() CommonFlags {
	var f CommonFlags
	flag.StringVar(&f.Base, "base", "", "fichier vecteurs de base (JSONL ou fvecs)")
	flag.StringVar(&f.Queries, "queries", "", "fichier requêtes (JSONL ou fvecs)")
	flag.StringVar(&f.GT, "gt", "", "fichier groundtruth (ivecs ou .gt.json), optionnel")
	flag.IntVar(&f.K, "k", 10, "nombre de voisins recherchés")
	flag.IntVar(&f.Limit, "limit", 0, "tronquer la base aux N premiers vecteurs (0=tout)")
	flag.StringVar(&f.Sweep, "sweep", "64,128,256,512", "valeurs balayées (EfSearch/ef)")
	flag.IntVar(&f.Holdout, "holdout", 0, "si -queries==-base, retirer les N derniers vecteurs comme requêtes")
	flag.Parse()
	return f
}

// Validate vérifie les flags obligatoires.
func (f CommonFlags) Validate() error {
	if f.Base == "" {
		return fmt.Errorf("flag -base requis")
	}
	if f.Queries == "" {
		return fmt.Errorf("flag -queries requis")
	}
	if f.K <= 0 {
		return fmt.Errorf("flag -k doit être > 0")
	}
	return nil
}
