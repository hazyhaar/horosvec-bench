package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// maxTitleLen borne la longueur d'un titre restitué (troncature à l'ellipse) : la page ne rend
// qu'un extrait lisible, jamais un texte arbitrairement long.
const maxTitleLen = 160

// loadTitles charge un fichier optionnel de titres au format « id<TAB>titre » (une ligne par
// item, séparateur tabulation). Un chemin vide retourne une map nil (la page affiche alors
// l'id HN et son lien, repli assumé de la V1). Les titres sont tronqués à maxTitleLen.
func loadTitles(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	titles := make(map[string]string)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Text()
		if raw == "" {
			continue
		}
		id, title, ok := strings.Cut(raw, "\t")
		if !ok {
			return nil, fmt.Errorf("titres ligne %d : séparateur tabulation absent", line)
		}
		id = strings.TrimSpace(id)
		title = strings.TrimSpace(title)
		if id == "" || title == "" {
			continue
		}
		titles[id] = truncateTitle(title)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return titles, nil
}

// truncateTitle tronque un titre à maxTitleLen runes, en ajoutant une ellipse le cas échéant.
func truncateTitle(s string) string {
	r := []rune(s)
	if len(r) <= maxTitleLen {
		return s
	}
	return string(r[:maxTitleLen]) + "…"
}
