package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Constantes de back-off 429, reprises à l'identique du banc de calibrage
// (/inference/benches/embed_calibrage_20260707.py) : le serveur llama-swap renvoie
// HTTP 429 au-delà d'une certaine concurrence (slots bornés). Le client attend son tour
// (back-off exponentiel borné) ; tout autre code HTTP reste fail-loud.
const (
	backoffInitial = 250 * time.Millisecond
	backoffCap     = 4 * time.Second
	maxAttempts    = 8                 // 1 tentative + 7 retries, comme le banc python
	httpTimeout    = 300 * time.Second // couvre le warmup du slot (chargement 1-3 min)
)

// embedClient poste des lots de textes vers l'endpoint OpenAI-compatible /v1/embeddings.
type embedClient struct {
	endpoint string
	model    string
	dims     int // 0 = dimension native du modèle ; >0 = MRL demandé au serveur
	http     *http.Client
}

func newEmbedClient(endpoint, model string, dims int) *embedClient {
	return &embedClient{
		endpoint: endpoint,
		model:    model,
		dims:     dims,
		http:     &http.Client{Timeout: httpTimeout},
	}
}

type embedRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions *int     `json:"dimensions,omitempty"`
	// TruncatePromptTokens fait tronquer PAR LE SERVEUR (vLLM) tout texte au-delà de
	// N tokens. Indispensable : le plafond client en octets (maxTextBytes) ne borne pas
	// les tokens — un texte HN saturé d'entités HTML dépasse 8192 tokens sous 24k octets
	// (incident run 26,7M du 2026-07-08, HTTP 400 à 465k docs). Vérifié par sonde :
	// input 9000 mots → usage.prompt_tokens = 8192, embedding rendu.
	TruncatePromptTokens int `json:"truncate_prompt_tokens,omitempty"`
}

type embedResponseItem struct {
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type embedResponse struct {
	Data []embedResponseItem `json:"data"`
}

// embedBatch poste un lot et retourne les vecteurs RÉORDONNÉS par le champ `index` de la
// réponse (l'ordre du tableau data n'est pas contractuel). Back-off borné sur 429, fail-loud
// sur tout autre statut ou toute incohérence de forme.
func (c *embedClient) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	// 8000 et non 8192 (max_model_len) : une séquence tronquée à EXACTEMENT la fenêtre
	// du modèle déclenche une course d'ordonnanceur vLLM sous concurrence — la requête
	// reste en Waiting sans jamais être servie (mesuré au sol 2026-07-08 : 3 pendues/24
	// à 8192, 0/24 à 8000, mêmes lots réels, concurrence 8, direct :8005).
	reqBody := embedRequest{Model: c.model, Input: texts, TruncatePromptTokens: 8000}
	if c.dims > 0 {
		d := c.dims
		reqBody.Dimensions = &d
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("hnbook-embed: marshal request: %w", err)
	}

	backoff := backoffInitial
	for attempt := 0; attempt < maxAttempts; attempt++ {
		vecs, retryable, err := c.attempt(ctx, payload, len(texts))
		if err == nil {
			return vecs, nil
		}
		if !retryable || attempt == maxAttempts-1 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > backoffCap {
			backoff = backoffCap
		}
	}
	return nil, fmt.Errorf("hnbook-embed: 429 persistant après %d tentatives", maxAttempts)
}

// attempt exécute une tentative HTTP. retryable=true uniquement sur 429.
func (c *embedClient) attempt(ctx context.Context, payload []byte, n int) (vecs [][]float32, retryable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, false, fmt.Errorf("hnbook-embed: new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("hnbook-embed: http do: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("hnbook-embed: read body: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, true, fmt.Errorf("hnbook-embed: HTTP 429")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("hnbook-embed: HTTP %d: %s", resp.StatusCode, truncate(body, 400))
	}

	var er embedResponse
	if err := json.Unmarshal(body, &er); err != nil {
		return nil, false, fmt.Errorf("hnbook-embed: parse response: %w", err)
	}
	if len(er.Data) != n {
		return nil, false, fmt.Errorf("hnbook-embed: réponse %d vecteurs pour %d textes", len(er.Data), n)
	}
	out := make([][]float32, n)
	for _, item := range er.Data {
		if item.Index < 0 || item.Index >= n {
			return nil, false, fmt.Errorf("hnbook-embed: index de réponse %d hors bornes [0,%d)", item.Index, n)
		}
		if out[item.Index] != nil {
			return nil, false, fmt.Errorf("hnbook-embed: index de réponse %d dupliqué", item.Index)
		}
		out[item.Index] = item.Embedding
	}
	for i, v := range out {
		if v == nil {
			return nil, false, fmt.Errorf("hnbook-embed: vecteur manquant à l'index %d", i)
		}
	}
	return out, false, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}
