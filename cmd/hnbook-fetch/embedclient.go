package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	embedDim             = 512
	maxEmbedTextBytes    = 2000
	defaultEmbedURL      = "http://127.0.0.1:8471/embed"
	defaultHNBaseURL     = "https://hacker-news.firebaseio.com/v0"
	defaultBackfillStart = 28700000
)

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

func newEmbedClient(url string, hc *http.Client) *embedClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &embedClient{url: url, hc: hc}
}

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
