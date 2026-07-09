package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// minTruncationMargin est la marge minimale, en tokens, exigée entre la borne de
// troncature du client (truncatePromptTokens) et la fenêtre pleine du modèle servi
// (max_model_len). Tronquer À la fenêtre pleine — ou à moins d'une poignée de tokens
// en dessous — rouvre la course d'ordonnanceur vLLM de l'incident pooling 2026-07-08
// (requêtes bloquées en Waiting, jamais servies). 128 tokens de marge écartent ce
// régime tout en restant négligeables devant une fenêtre de plusieurs milliers.
const minTruncationMargin = 128

// marginVerdict est le résultat de la décision de marge de fenêtre (G3).
type marginVerdict int

const (
	// marginOK : la borne de troncature laisse au moins minTruncationMargin sous la fenêtre.
	marginOK marginVerdict = iota
	// marginRefuse : marge insuffisante — le run DOIT être refusé fail-loud.
	marginRefuse
	// marginUnverifiable : le slot ne publie pas de max_model_len exploitable — Warn
	// explicite, jamais un vert silencieux.
	marginUnverifiable
)

// decideMargin tranche la sûreté de la marge de fenêtre. maxModelLen <= 0 signifie
// « non publié » (verdict invérifiable). Sinon la marge exigée est
// truncateBound + minTruncationMargin <= maxModelLen ; en deçà, refus.
//
// Fonction PURE (aucune E/S) : c'est l'unité testée par l'oracle G3.
func decideMargin(truncateBound, maxModelLen int) marginVerdict {
	if maxModelLen <= 0 {
		return marginUnverifiable
	}
	if truncateBound+minTruncationMargin > maxModelLen {
		return marginRefuse
	}
	return marginOK
}

// modelsResponse reflète la forme OpenAI-compatible de GET /v1/models. max_model_len
// est publié par vLLM ; absent (0) chez d'autres serveurs → invérifiable.
type modelsResponse struct {
	Data []struct {
		ID          string `json:"id"`
		MaxModelLen int    `json:"max_model_len"`
	} `json:"data"`
}

// modelsURLFromEmbeddings dérive l'URL /v1/models de l'endpoint /v1/embeddings.
func modelsURLFromEmbeddings(endpoint string) string {
	if strings.HasSuffix(endpoint, "/embeddings") {
		return strings.TrimSuffix(endpoint, "/embeddings") + "/models"
	}
	// Repli : tenter un frère /v1/models à la racine de l'hôte n'étant pas fiable,
	// on renvoie l'endpoint tel quel — fetchMaxModelLen rendra alors invérifiable.
	return endpoint
}

// maxModelsBody borne la lecture du corps de /v1/models (quelques Mo largement
// suffisants) — aligné sur la convention bornée du module (cmd/hnbook-serve/embedclient.go).
const maxModelsBody = 1 << 20

// fetchMaxModelLen interroge GET /v1/models et retourne le max_model_len publié pour le
// modèle demandé. Si le modèle n'est pas trouvé nommément, replie sur le premier item
// publiant une fenêtre en émettant un Warn explicite (la marge évaluée n'est alors pas
// représentative du modèle cible — jamais un vert muet). Retourne 0 sans erreur quand
// rien d'exploitable n'est publié (verdict invérifiable en aval) ; une erreur seulement
// sur défaillance réseau/protocole.
func fetchMaxModelLen(ctx context.Context, endpoint, model string, log *slog.Logger) (int, error) {
	url := modelsURLFromEmbeddings(endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("hnbook-embed: requête /v1/models: %w", err)
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("hnbook-embed: appel /v1/models: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	if err != nil {
		return 0, fmt.Errorf("hnbook-embed: lecture /v1/models: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("hnbook-embed: /v1/models HTTP %d", resp.StatusCode)
	}
	var mr modelsResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return 0, fmt.Errorf("hnbook-embed: parse /v1/models: %w", err)
	}
	for _, m := range mr.Data {
		if m.ID == model && m.MaxModelLen > 0 {
			return m.MaxModelLen, nil
		}
	}
	// Modèle non trouvé nommément : replier sur le premier item publiant une fenêtre,
	// mais le signaler — la marge n'est alors pas garantie représentative du modèle servi.
	for _, m := range mr.Data {
		if m.MaxModelLen > 0 {
			log.Warn("garde fenêtre : modèle non trouvé nommément dans /v1/models, marge évaluée sur un item de repli",
				"modele_demande", model, "item_repli", m.ID, "max_model_len", m.MaxModelLen)
			return m.MaxModelLen, nil
		}
	}
	return 0, nil
}

// guardWindowMargin est la garde G3 exécutée au démarrage de hnbook-embed : elle
// interroge le slot servant, décide de la marge, et REFUSE fail-loud si la borne de
// troncature ne laisse pas minTruncationMargin sous la fenêtre pleine du modèle. Une
// fenêtre non publiée (ou un slot injoignable) produit un Warn explicite, jamais un vert
// silencieux ni un blocage.
func guardWindowMargin(ctx context.Context, endpoint, model string, log *slog.Logger) error {
	maxLen, err := fetchMaxModelLen(ctx, endpoint, model, log)
	if err != nil {
		log.Warn("garde fenêtre : max_model_len non vérifiable (slot injoignable ou muet)",
			"endpoint", endpoint, "err", err.Error(),
			"borne_troncature", truncatePromptTokens, "marge_min", minTruncationMargin)
		return nil
	}
	switch decideMargin(truncatePromptTokens, maxLen) {
	case marginUnverifiable:
		log.Warn("garde fenêtre : le slot ne publie pas max_model_len — marge non vérifiable",
			"endpoint", endpoint, "borne_troncature", truncatePromptTokens, "marge_min", minTruncationMargin)
		return nil
	case marginRefuse:
		return fmt.Errorf("garde fenêtre (G3) : borne de troncature %d à moins de %d tokens sous max_model_len=%d — "+
			"régime de course d'ordonnanceur vLLM (incident pooling 2026-07-08) ; run refusé",
			truncatePromptTokens, minTruncationMargin, maxLen)
	default:
		log.Info("garde fenêtre OK", "borne_troncature", truncatePromptTokens,
			"max_model_len", maxLen, "marge", maxLen-truncatePromptTokens)
		return nil
	}
}
