package protocol

import (
	"encoding/json"
	"fmt"
	"os"
)

// Result est un point de mesure émis en JSON (une ligne par point).
type Result struct {
	Engine  string `json:"engine"`
	Dataset string `json:"dataset"`
	Param   string `json:"param"`
	// Mode nomme le régime de mesure du moteur, jamais laissé implicite : pour
	// horosvec "arena" (rerank fp16 servi par l'arène) ou "db-blob" (rerank SQL
	// ligne à ligne, défaut) ; "native" pour hnsw ; "exact" pour sqlitevec. Ajouté
	// après l'incident 2026-07 (deux jours de mesures perdus sur un opt-in arène
	// silencieux : le mode ne figurait dans aucune ligne du JSONL).
	Mode string `json:"mode"`
	// Medium nomme le support de stockage réel du chemin de données (résolu via
	// pkg/storagemedium) : "rotational" | "ssd" | "unknown". Fail-soft : jamais
	// bloquant. Ajouté après l'incident 2026-07 (support de stockage non nommé,
	// latence ×100-370 non attribuable a posteriori).
	Medium      string  `json:"medium"`
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
