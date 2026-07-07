package data

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Dataset contient les vecteurs de base et les requêtes (disjoints).
type Dataset struct {
	Base    [][]float32
	Queries [][]float32
	Dim     int
	Name    string
}

// Load charge base et requêtes selon les chemins fournis.
// Si basePath == queriesPath, holdout retire les N derniers vecteurs de la base comme requêtes.
func Load(basePath, queriesPath string, limit, holdout int) (Dataset, error) {
	if basePath == "" {
		return Dataset{}, fmt.Errorf("data: -base requis")
	}
	if queriesPath == "" {
		return Dataset{}, fmt.Errorf("data: -queries requis")
	}

	sameFile := filepath.Clean(basePath) == filepath.Clean(queriesPath)
	if sameFile {
		if holdout <= 0 {
			return Dataset{}, fmt.Errorf("data: -queries == -base sans -holdout : les requêtes seraient dans la base")
		}
		all, err := loadVectors(basePath, 0)
		if err != nil {
			return Dataset{}, err
		}
		if holdout >= len(all) {
			return Dataset{}, fmt.Errorf("data: -holdout %d >= nombre de vecteurs %d", holdout, len(all))
		}
		split := len(all) - holdout
		if limit > 0 && limit < split {
			split = limit
		}
		ds := Dataset{
			Base:    all[:split],
			Queries: all[split:],
			Dim:     len(all[0]),
			Name:    filepath.Base(basePath),
		}
		if err := ds.validate(); err != nil {
			return Dataset{}, err
		}
		return ds, nil
	}

	base, err := loadVectors(basePath, limit)
	if err != nil {
		return Dataset{}, fmt.Errorf("data: base %q: %w", basePath, err)
	}
	queries, err := loadVectors(queriesPath, 0)
	if err != nil {
		return Dataset{}, fmt.Errorf("data: queries %q: %w", queriesPath, err)
	}
	ds := Dataset{
		Base:    base,
		Queries: queries,
		Dim:     len(base[0]),
		Name:    filepath.Base(basePath),
	}
	if err := ds.validate(); err != nil {
		return Dataset{}, err
	}
	return ds, nil
}

func (d Dataset) validate() error {
	if len(d.Base) == 0 {
		return fmt.Errorf("data: base vide")
	}
	if len(d.Queries) == 0 {
		return fmt.Errorf("data: requêtes vides")
	}
	dim := len(d.Base[0])
	if dim == 0 {
		return fmt.Errorf("data: dimension nulle")
	}
	for i, v := range d.Base {
		if len(v) != dim {
			return fmt.Errorf("data: base[%d] dim=%d, attendu %d", i, len(v), dim)
		}
	}
	for i, v := range d.Queries {
		if len(v) != dim {
			return fmt.Errorf("data: query[%d] dim=%d, attendu %d", i, len(v), dim)
		}
	}
	return nil
}

func loadVectors(path string, limit int) ([][]float32, error) {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".jsonl", ".json", ".ndjson":
		return loadJSONL(path, limit)
	case ".fvecs":
		return loadFvecs(path, limit)
	default:
		// Tenter JSONL si pas d'extension reconnue.
		if strings.Contains(path, "fvecs") || strings.HasSuffix(path, ".fvecs") {
			return loadFvecs(path, limit)
		}
		return loadJSONL(path, limit)
	}
}

// loadJSONL lit un vecteur JSON (tableau de float) par ligne.
func loadJSONL(path string, limit int) ([][]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	var out [][]float32
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for sc.Scan() {
		if limit > 0 && len(out) >= limit {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw []float64
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			return nil, fmt.Errorf("ligne %d: %w", len(out)+1, err)
		}
		vec := make([]float32, len(raw))
		for i, v := range raw {
			vec[i] = float32(v)
		}
		out = append(out, vec)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}
	return out, nil
}

// loadFvecs lit le format texmex fvecs (little-endian, dim int32 préfixée).
func loadFvecs(path string, limit int) ([][]float32, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	var out [][]float32
	var dim int
	for {
		if limit > 0 && len(out) >= limit {
			break
		}
		vec, d, err := readFvec(f)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(out) == 0 {
			dim = d
		} else if d != dim {
			return nil, fmt.Errorf("fvecs: dim hétérogène à l'index %d: %d != %d", len(out), d, dim)
		}
		out = append(out, vec)
	}
	return out, nil
}

func readFvec(r io.Reader) ([]float32, int, error) {
	var dim int32
	if err := binary.Read(r, binary.LittleEndian, &dim); err != nil {
		return nil, 0, err
	}
	if dim <= 0 || dim > 1_000_000 {
		return nil, 0, fmt.Errorf("fvecs: dim invalide %d", dim)
	}
	vec := make([]float32, dim)
	if err := binary.Read(r, binary.LittleEndian, vec); err != nil {
		return nil, 0, fmt.Errorf("fvecs: lecture vecteur: %w", err)
	}
	return vec, int(dim), nil
}

// LoadIvecs lit un fichier groundtruth texmex ivecs.
func LoadIvecs(path string) ([][]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	var out [][]int
	for {
		neighbors, err := readIvec(f)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		out = append(out, neighbors)
	}
	return out, nil
}

func readIvec(r io.Reader) ([]int, error) {
	var k int32
	if err := binary.Read(r, binary.LittleEndian, &k); err != nil {
		return nil, err
	}
	if k <= 0 || k > 1_000_000 {
		return nil, fmt.Errorf("ivecs: k invalide %d", k)
	}
	raw := make([]int32, k)
	if err := binary.Read(r, binary.LittleEndian, raw); err != nil {
		return nil, fmt.Errorf("ivecs: lecture voisins: %w", err)
	}
	out := make([]int, k)
	for i, v := range raw {
		out[i] = int(v)
	}
	return out, nil
}

// L2Distance calcule la distance L2 euclidienne.
func L2Distance(a, b []float32) float64 {
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return math.Sqrt(sum)
}
