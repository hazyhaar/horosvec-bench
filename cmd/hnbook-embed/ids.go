package main

import (
	"encoding/binary"
	"fmt"
	"os"
)

// idBytes est la taille fixe d'un id sur disque : uint64 little-endian. Un stride fixe rend
// le fichier d'ids symétrique de l'arène (offset = rang × idBytes), donc tronquable
// exactement au checkpoint sans rescan — condition de l'appariement arène↔ids sous reprise.
const idBytes = 8

// idsWriter écrit les ids HN (uint64) au fil de l'eau, un par rang, dans path+".tmp".
// finalize renomme atomiquement vers path.
type idsWriter struct {
	f     *os.File
	tmp   string
	path  string
	count int64
	buf   [idBytes]byte
}

// newIDsWriter crée le fichier temporaire d'ids (départ à zéro).
func newIDsWriter(path string) (*idsWriter, error) {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return nil, fmt.Errorf("hnbook-embed: ids writer create: %w", err)
	}
	return &idsWriter{f: f, tmp: tmp, path: path}, nil
}

// resumeIDsWriter rouvre le fichier d'ids en cours au rang count, tronquant tout octet
// partiel au-delà (symétrique de ResumeArenaWriter).
func resumeIDsWriter(path string, count int64) (*idsWriter, error) {
	if count < 0 {
		return nil, fmt.Errorf("hnbook-embed: ids writer resume: invalid count %d", count)
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("hnbook-embed: ids writer resume open: %w", err)
	}
	want := count * idBytes
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("hnbook-embed: ids writer resume stat: %w", err)
	}
	if st.Size() < want {
		f.Close()
		return nil, fmt.Errorf("hnbook-embed: ids writer resume: size %d shorter than checkpoint %d", st.Size(), want)
	}
	if err := f.Truncate(want); err != nil {
		f.Close()
		return nil, fmt.Errorf("hnbook-embed: ids writer resume truncate: %w", err)
	}
	if _, err := f.Seek(want, 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("hnbook-embed: ids writer resume seek: %w", err)
	}
	return &idsWriter{f: f, tmp: tmp, path: path, count: count}, nil
}

// write append un id au rang courant.
func (w *idsWriter) write(id uint64) error {
	binary.LittleEndian.PutUint64(w.buf[:], id)
	if _, err := w.f.Write(w.buf[:]); err != nil {
		return fmt.Errorf("hnbook-embed: ids writer write: %w", err)
	}
	w.count++
	return nil
}

// sync durcit le fichier d'ids sur disque (point de checkpoint).
func (w *idsWriter) sync() error {
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("hnbook-embed: ids writer sync: %w", err)
	}
	return nil
}

// finalize fsync et renomme atomiquement vers le chemin final.
func (w *idsWriter) finalize() error {
	if err := w.f.Sync(); err != nil {
		w.abort()
		return fmt.Errorf("hnbook-embed: ids writer finalize sync: %w", err)
	}
	if err := w.f.Close(); err != nil {
		os.Remove(w.tmp)
		return fmt.Errorf("hnbook-embed: ids writer close: %w", err)
	}
	if err := os.Rename(w.tmp, w.path); err != nil {
		os.Remove(w.tmp)
		return fmt.Errorf("hnbook-embed: ids writer rename: %w", err)
	}
	return nil
}

// abort ferme et supprime le fichier temporaire.
func (w *idsWriter) abort() {
	w.f.Close()
	os.Remove(w.tmp)
}

// readIDs lit un fichier d'ids finalisé (uint64 LE, stride fixe). Utilisé par le smoke pour
// reconstruire la correspondance rang→id HN.
func readIDs(path string) ([]uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("hnbook-embed: read ids: %w", err)
	}
	if len(data)%idBytes != 0 {
		return nil, fmt.Errorf("hnbook-embed: ids file %s taille %d non multiple de %d", path, len(data), idBytes)
	}
	n := len(data) / idBytes
	out := make([]uint64, n)
	for i := 0; i < n; i++ {
		out[i] = binary.LittleEndian.Uint64(data[i*idBytes:])
	}
	return out, nil
}
