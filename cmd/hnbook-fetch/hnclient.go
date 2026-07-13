package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
)

type hnItem struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Time    int64  `json:"time"`
	Title   string `json:"title"`
	Text    string `json:"text"`
	Parent  int64  `json:"parent"`
	Dead    bool   `json:"dead"`
	Deleted bool   `json:"deleted"`
}

type hnClient struct {
	baseURL string
	hc      *http.Client
	sem     chan struct{}
}

func newHNClient(baseURL string, hc *http.Client, concurrency int) *hnClient {
	if hc == nil {
		hc = http.DefaultClient
	}
	if concurrency < 1 {
		concurrency = 1
	}
	return &hnClient{
		baseURL: baseURL,
		hc:      hc,
		sem:     make(chan struct{}, concurrency),
	}
}

func (c *hnClient) MaxItem(ctx context.Context) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/maxitem.json", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("maxitem: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return 0, fmt.Errorf("maxitem statut %d: %s", resp.StatusCode, body)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(string(body), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("maxitem parse: %w", err)
	}
	return n, nil
}

func (c *hnClient) FetchItem(ctx context.Context, id int64) (*hnItem, error) {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()

	url := fmt.Sprintf("%s/item/%d.json", c.baseURL, id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("item %d: %w", id, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("item %d statut %d: %s", id, resp.StatusCode, body)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if string(raw) == "null" {
		return nil, nil
	}
	var item hnItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("item %d decode: %w", id, err)
	}
	if item.ID == 0 {
		item.ID = id
	}
	return &item, nil
}

func (item *hnItem) skip() bool {
	return item == nil || item.Dead || item.Deleted
}

func (item *hnItem) embedText() string {
	if item.Text != "" {
		return item.Text
	}
	return item.Title
}

func (item *hnItem) ndjsonLine() string {
	row := struct {
		ID     int64  `json:"id"`
		TS     int64  `json:"ts"`
		Type   string `json:"type"`
		Title  string `json:"title"`
		Parent int64  `json:"parent"`
		Text   string `json:"text"`
	}{
		ID: item.ID, TS: item.Time, Type: item.Type,
		Title: item.Title, Parent: item.Parent, Text: item.Text,
	}
	b, _ := json.Marshal(row)
	return string(b)
}

func hnExtID(id int64) []byte {
	return []byte(strconv.FormatInt(id, 10))
}

// prefetchMap charge en parallèle les items pour les ids demandés (ordre non garanti).
func (c *hnClient) prefetchMap(ctx context.Context, ids []int64) map[int64]*hnItem {
	out := make(map[int64]*hnItem, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			item, err := c.FetchItem(ctx, id)
			if err != nil {
				return
			}
			mu.Lock()
			out[id] = item
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return out
}
