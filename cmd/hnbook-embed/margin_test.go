package main

import "testing"

// TestDecideMargin couvre les trois régimes de la garde de fenêtre (G3) : marge
// suffisante, marge insuffisante (borne trop proche ou au-dessus de la fenêtre), et
// fenêtre non publiée.
func TestDecideMargin(t *testing.T) {
	cases := []struct {
		name        string
		truncate    int
		maxModelLen int
		want        marginVerdict
	}{
		{"marge large", 8000, 8192, marginOK},                       // 192 >= 128
		{"marge exacte au seuil", 8000, 8128, marginOK},             // 128 == marge min → OK
		{"marge insuffisante", 8000, 8100, marginRefuse},            // 100 < 128
		{"borne egale fenetre", 8192, 8192, marginRefuse},           // 0 de marge
		{"borne au dessus fenetre", 8300, 8192, marginRefuse},       // dépassement
		{"fenetre non publiee (zero)", 8000, 0, marginUnverifiable}, // non publié
		{"fenetre non publiee (negatif)", 8000, -1, marginUnverifiable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := decideMargin(c.truncate, c.maxModelLen); got != c.want {
				t.Fatalf("decideMargin(%d,%d) = %d ; attendu %d", c.truncate, c.maxModelLen, got, c.want)
			}
		})
	}
}

// TestDecideMarginRealBound vérifie la décision avec la borne de production réelle
// (truncatePromptTokens) contre une fenêtre qwen3-embedding typique (8192).
func TestDecideMarginRealBound(t *testing.T) {
	if got := decideMargin(truncatePromptTokens, 8192); got != marginOK {
		t.Fatalf("borne de production %d contre fenêtre 8192 devrait être OK ; got %d", truncatePromptTokens, got)
	}
}

// TestModelsURLFromEmbeddings vérifie la dérivation de l'URL /v1/models.
func TestModelsURLFromEmbeddings(t *testing.T) {
	got := modelsURLFromEmbeddings("http://127.0.0.1:8001/v1/embeddings")
	if want := "http://127.0.0.1:8001/v1/models"; got != want {
		t.Fatalf("modelsURLFromEmbeddings = %q ; attendu %q", got, want)
	}
}
