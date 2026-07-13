package main

import (
	"html"
	"regexp"
	"strings"
)

// maxTitleLen borne la longueur d'un titre restitué (troncature à l'ellipse) : la page ne rend
// qu'un extrait lisible, jamais un texte arbitrairement long.
const maxTitleLen = 160

// maxTextSnippetLen borne l'aperçu inline d'un commentaire dans /api/search.
const maxTextSnippetLen = 140

var hnParagraphRe = regexp.MustCompile(`(?i)</?p>`)

// hnTagRe capture tout élément de balisage HN résiduel (<a href…>, <i>, <pre>, <code>…)
// pour le retirer de l'aperçu : le texte brut lisible, jamais du markup affiché tel quel.
var hnTagRe = regexp.MustCompile(`<[^>]+>`)

// truncateTitle tronque un titre à maxTitleLen runes, en ajoutant une ellipse le cas échéant.
func truncateTitle(s string) string {
	r := []rune(s)
	if len(r) <= maxTitleLen {
		return s
	}
	return string(r[:maxTitleLen]) + "…"
}

// decodeHNText convertit le balisage/entités HN en texte brut (anti-injection côté rendu).
func decodeHNText(raw string) string {
	if raw == "" {
		return ""
	}
	s := hnParagraphRe.ReplaceAllString(raw, "\n\n")
	// Retirer le markup AVANT de déséchapper les entités : les balises sont littérales dans
	// le texte HN (<a href="…">), tandis que &gt;/&quot; sont encodées — déséchapper d'abord
	// ferait réapparaître des chevrons pris pour du balisage.
	s = hnTagRe.ReplaceAllString(s, "")
	return strings.TrimSpace(html.UnescapeString(s))
}

// truncateTextSnippet rend les premiers maxTextSnippetLen caractères du texte HN décodé.
func truncateTextSnippet(raw string) string {
	decoded := decodeHNText(raw)
	r := []rune(decoded)
	if len(r) <= maxTextSnippetLen {
		return string(r)
	}
	return string(r[:maxTextSnippetLen]) + "…"
}
