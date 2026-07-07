package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// manifestMagic identifie le manifest de pipeline hnbook-embed (distinct du header d'arène).
const manifestMagic = "HNBOOK_EMBED_MANIFEST1"

// statusInProgress / statusDone : état du run porté par le manifest.
const (
	statusInProgress = "in_progress"
	statusDone       = "done"
)

// Manifest est l'état durable du pipeline, écrit atomiquement à chaque checkpoint. Le champ
// Count est le nombre de vecteurs DURABLES (fsync arène+ids confirmé avant l'écriture du
// manifest) : c'est le point de reprise. Il ne dépasse jamais les octets réellement sur
// disque, ce qui garantit qu'une reprise ne crée ni doublon ni trou.
type Manifest struct {
	Magic     string `json:"magic"`
	Model     string `json:"model"`
	Dim       int    `json:"dim"`
	ArenaPath string `json:"arena_path"`
	IDsPath   string `json:"ids_path"`
	Count     int64  `json:"count"`
	Status    string `json:"status"`
	UpdatedAt string `json:"updated_at"`
}

// readManifest lit un manifest existant. Retourne (nil, nil) si le fichier n'existe pas
// (aucun run antérieur — départ à zéro).
func readManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("hnbook-embed: read manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("hnbook-embed: parse manifest %s: %w", path, err)
	}
	if m.Magic != manifestMagic {
		return nil, fmt.Errorf("hnbook-embed: manifest %s: bad magic %q", path, m.Magic)
	}
	return &m, nil
}

// writeManifest écrit le manifest de façon atomique (fichier temporaire, fsync, rename).
// Un crash pendant l'écriture laisse l'ancien manifest intact.
func writeManifest(path string, m Manifest) error {
	m.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("hnbook-embed: marshal manifest: %w", err)
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("hnbook-embed: manifest create: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("hnbook-embed: manifest write: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("hnbook-embed: manifest sync: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("hnbook-embed: manifest close: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("hnbook-embed: manifest rename: %w", err)
	}
	return nil
}
