package protocol

import (
	"encoding/json"
	"fmt"
	"os"
)

// Result est un point de mesure émis en JSON (une ligne par point).
type Result struct {
	Engine      string  `json:"engine"`
	Dataset     string  `json:"dataset"`
	Param       string  `json:"param"`
	N           int     `json:"n"`
	Dim         int     `json:"dim"`
	K           int     `json:"k"`
	Concurrency int     `json:"concurrency"`
	BuildS      float64 `json:"build_s"`
	InsertQPS   float64 `json:"insert_qps"`
	RecallMean  float64 `json:"recall_mean"`
	RecallMin   float64 `json:"recall_min"`
	QPS         float64 `json:"qps"`
	P50Ms       float64 `json:"p50_ms"`
	P99Ms       float64 `json:"p99_ms"`
	MemMB       float64 `json:"mem_mb"`
}

// Emit écrit un Result sur stdout en JSONL.
func Emit(r Result) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("protocol: marshal result: %w", err)
	}
	_, err = fmt.Fprintln(os.Stdout, string(b))
	if err != nil {
		return fmt.Errorf("protocol: write stdout: %w", err)
	}
	return nil
}
