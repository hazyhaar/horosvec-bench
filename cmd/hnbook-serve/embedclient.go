package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// embedDim est la dimension du vecteur de requête attendue du sidecar (troncature Matryoshka
// à 512, conforme au pipeline de référence embed_queries_cpu.py) et servie par l'index.
const embedDim = 512

// maxEmbedTextBytes borne la longueur du texte transmis au sidecar. Le serveur borne déjà la
// requête à 512 octets ; cette marge relaie la même intention côté client d'embedding.
const maxEmbedTextBytes = 2000

// embedClient interroge le sidecar d'embedding local en HTTP : un texte en entrée, un vecteur
// normalisé de dimension embedDim en sortie. Toute défaillance (sidecar mort, réponse
// malformée, dimension inattendue) remonte une erreur — jamais un vecteur silencieusement vide.
type embedClient struct {
	url string
	hc  *http.Client
}

type embedRequest struct {
	Text string `json:"text"`
}

type embedResponse struct {
	Vector []float32 `json:"vector"`
}

// embed transmet le texte au sidecar et retourne le vecteur de requête. Le contexte porte le
// délai d'expiration de la requête HTTP (fail-loud à l'échéance).
func (e *embedClient) embed(ctx context.Context, text string) ([]float32, error) {
	if len(text) > maxEmbedTextBytes {
		text = text[:maxEmbedTextBytes]
	}
	body, err := json.Marshal(embedRequest{Text: text})
	if err != nil {
		return nil, fmt.Errorf("encodage requête embedding: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("construction requête embedding: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("appel sidecar embedding: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("sidecar embedding statut %d: %s", resp.StatusCode, bytes.TrimSpace(snippet))
	}

	var er embedResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&er); err != nil {
		return nil, fmt.Errorf("décodage réponse embedding: %w", err)
	}
	if len(er.Vector) != embedDim {
		return nil, fmt.Errorf("dimension embedding %d != %d attendue", len(er.Vector), embedDim)
	}
	return er.Vector, nil
}
