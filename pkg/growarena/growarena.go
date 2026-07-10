// Package growarena — PROTOTYPE R&D (code assumé jetable, hors moteur horosvec).
//
// Magasin de vecteurs fp16 keyé par node_id dense, à la fois :
//   - lecture concurrente sans verrou (arithmétique d'offset sur des mmap immuables),
//   - inscriptible en incrémental (écrivain unique, append en segment de queue),
//   - persistant, réouvrable après crash (compteur committed par segment).
//
// Patron : magasin SEGMENTÉ append-only. Chaque segment est un fichier PRÉ-ALLOUÉ
// à sa capacité pleine et cartographié (mmap MAP_SHARED) UNE SEULE FOIS à la
// création — il n'est JAMAIS remappé ni retaillé, donc aucun pointeur de lecteur
// en vol n'est jamais invalidé. La croissance ne passe pas par mremap/ftruncate
// d'une zone vivante : elle passe par l'AJOUT d'un segment neuf, publié par un
// swap de table (atomic.Pointer). La visibilité d'un vecteur fraîchement écrit
// est gouvernée par un compteur atomique global (release-store après l'écriture
// des octets, acquire-load côté lecteur), jamais par la taille du fichier.
//
// Modèle mémoire (le cœur du verrou technique) :
//   - écrivain : copie les octets fp16 dans la zone mmap du segment de queue,
//     PUIS count.Store(n+1) (release). Un lecteur qui observe count >= n+1
//     observe donc des octets pleinement écrits (happens-before de sync/atomic).
//   - lecteur : count.Load() (acquire), refuse tout node_id >= count, puis lit
//     par offset. Aucune lecture déchirée possible : les octets d'un vecteur ne
//     sont publiés qu'une fois complets, et ne sont jamais réécrits (append-only).
//   - table des segments : atomic.Pointer sur un slice immuable ; l'écrivain
//     publie une COPIE étendue, les anciens lecteurs gardent l'ancienne vue
//     (toujours valide : les segments ne sont jamais unmappés en vie de process).
//
// Durabilité : Flush() msync chaque segment sale puis grave le compteur
// `committed` dans le header du segment (write-8-octets + msync de la page
// header). À la réouverture, seuls les vecteurs <= committed sont visibles ;
// un append non flushé avant crash est perdu (contrat append-only classique,
// même sémantique qu'un WAL non commité).
package growarena

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const (
	magic      = "HVGROW01"
	headerSize = 32 // magic(8) + version(4) + dim(4) + capacity(8) + committed(8)
	version    = 1
)

type segment struct {
	path string
	data []byte // mmap RW MAP_SHARED, taille headerSize + cap*dim*2, jamais remappé
	cap  int64
}

// Store est le magasin segmenté. Écrivain UNIQUE (Append/Flush non réentrants) ;
// lecteurs illimités sans verrou (Get).
type Store struct {
	dir    string
	dim    int
	segCap int64 // vecteurs par segment

	count atomic.Int64               // vecteurs visibles (publication release/acquire)
	segs  atomic.Pointer[[]*segment] // table immuable, swap à l'ajout de segment
	dirty map[int]bool               // segments à msync (écrivain seul)
}

// Open crée ou rouvre un magasin dans dir. segCap est la capacité d'un segment
// en vecteurs (granularité de croissance). À la réouverture, la visibilité est
// restaurée au dernier committed flushé de chaque segment.
func Open(dir string, dim int, segCap int64) (*Store, error) {
	if dim <= 0 || dim > 1<<20 || segCap <= 0 {
		return nil, fmt.Errorf("growarena: invalid dim=%d segCap=%d", dim, segCap)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, dim: dim, segCap: segCap, dirty: map[int]bool{}}
	empty := []*segment{}
	s.segs.Store(&empty)

	var total int64
	for i := 0; ; i++ {
		p := s.segPath(i)
		if _, err := os.Stat(p); err != nil {
			if os.IsNotExist(err) {
				break
			}
			return nil, err
		}
		seg, committed, err := openSegment(p, dim, segCap)
		if err != nil {
			return nil, err
		}
		s.appendSegTable(seg)
		total += committed
		if committed < segCap {
			break // segment de queue partiel : les suivants n'existent pas
		}
	}
	s.count.Store(total)
	return s, nil
}

func (s *Store) segPath(i int) string {
	return filepath.Join(s.dir, fmt.Sprintf("seg-%06d.hvg", i))
}

func (s *Store) appendSegTable(seg *segment) {
	old := *s.segs.Load()
	next := make([]*segment, len(old)+1)
	copy(next, old)
	next[len(old)] = seg
	s.segs.Store(&next)
}

// createSegment pré-alloue le fichier à sa taille PLEINE (ftruncate) et le mmap
// une seule fois. C'est l'invariant central : la zone ne bougera plus jamais.
func createSegment(path string, dim int, segCap int64) (*segment, error) {
	size := int64(headerSize) + segCap*int64(dim)*2
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		os.Remove(path)
		return nil, err
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(size),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	copy(data[0:8], magic)
	binary.LittleEndian.PutUint32(data[8:], version)
	binary.LittleEndian.PutUint32(data[12:], uint32(dim))
	binary.LittleEndian.PutUint64(data[16:], uint64(segCap))
	binary.LittleEndian.PutUint64(data[24:], 0) // committed
	return &segment{path: path, data: data, cap: segCap}, nil
}

func openSegment(path string, dim int, segCap int64) (*segment, int64, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	want := int64(headerSize) + segCap*int64(dim)*2
	if st.Size() != want {
		return nil, 0, fmt.Errorf("growarena: %s: size %d != %d", path, st.Size(), want)
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, int(want),
		syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, 0, err
	}
	if string(data[0:8]) != magic ||
		binary.LittleEndian.Uint32(data[8:]) != version ||
		int(binary.LittleEndian.Uint32(data[12:])) != dim ||
		int64(binary.LittleEndian.Uint64(data[16:])) != segCap {
		syscall.Munmap(data)
		return nil, 0, fmt.Errorf("growarena: %s: bad header", path)
	}
	committed := int64(binary.LittleEndian.Uint64(data[24:]))
	if committed < 0 || committed > segCap {
		syscall.Munmap(data)
		return nil, 0, fmt.Errorf("growarena: %s: committed %d out of range", path, committed)
	}
	return &segment{path: path, data: data, cap: segCap}, committed, nil
}

// Count renvoie le nombre de vecteurs visibles.
func (s *Store) Count() int64 { return s.count.Load() }

// Append ajoute un vecteur (écrivain unique). Les octets sont écrits AVANT la
// publication du compteur : un lecteur ne peut jamais voir un vecteur partiel.
func (s *Store) Append(vec []float32) (int64, error) {
	if len(vec) != s.dim {
		return 0, fmt.Errorf("growarena: append dim %d != %d", len(vec), s.dim)
	}
	id := s.count.Load() // seul l'écrivain avance count : lecture plain suffisante
	segIdx := int(id / s.segCap)
	segs := *s.segs.Load()
	if segIdx == len(segs) {
		seg, err := createSegment(s.segPath(segIdx), s.dim, s.segCap)
		if err != nil {
			return 0, err
		}
		s.appendSegTable(seg)
		segs = *s.segs.Load()
	}
	seg := segs[segIdx]
	off := headerSize + (id%s.segCap)*int64(s.dim)*2
	for i, v := range vec {
		binary.LittleEndian.PutUint16(seg.data[off+int64(i)*2:], Float32ToFloat16(v))
	}
	s.dirty[segIdx] = true
	s.count.Store(id + 1) // release : publie le vecteur complet
	return id, nil
}

// Get décode le vecteur fp16 de nodeID vers dst (len == dim). Lock-free.
func (s *Store) Get(nodeID int64, dst []float32) bool {
	if nodeID < 0 || nodeID >= s.count.Load() { // acquire
		return false
	}
	segs := *s.segs.Load()
	seg := segs[nodeID/s.segCap]
	off := headerSize + (nodeID%s.segCap)*int64(s.dim)*2
	for i := 0; i < s.dim; i++ {
		dst[i] = Float16ToFloat32(binary.LittleEndian.Uint16(seg.data[off+int64(i)*2:]))
	}
	return true
}

// Flush msync les segments sales puis grave et msync le compteur committed de
// chaque segment (durabilité du préfixe publié).
func (s *Store) Flush() error {
	n := s.count.Load()
	segs := *s.segs.Load()
	for idx := range s.dirty {
		seg := segs[idx]
		committed := n - int64(idx)*s.segCap
		if committed > seg.cap {
			committed = seg.cap
		}
		if committed < 0 {
			committed = 0
		}
		if err := msync(seg.data); err != nil {
			return fmt.Errorf("growarena: msync %s: %w", seg.path, err)
		}
		binary.LittleEndian.PutUint64(seg.data[24:], uint64(committed))
		if err := msync(seg.data[:headerSize]); err != nil {
			return fmt.Errorf("growarena: msync header %s: %w", seg.path, err)
		}
	}
	s.dirty = map[int]bool{}
	return nil
}

// Close flush puis unmap. À n'appeler qu'après l'arrêt de tous les lecteurs :
// l'unmap d'un segment sous un lecteur en vol serait un SIGSEGV — en vie de
// process, les segments ne sont JAMAIS unmappés (le coût : garder mappé ce qui
// a été mappé, négligeable en pages RÉSIDENTES car le page cache arbitre).
func (s *Store) Close() error {
	if err := s.Flush(); err != nil {
		return err
	}
	for _, seg := range *s.segs.Load() {
		if err := syscall.Munmap(seg.data); err != nil {
			return err
		}
	}
	empty := []*segment{}
	s.segs.Store(&empty)
	return nil
}

func msync(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	_, _, errno := syscall.Syscall(syscall.SYS_MSYNC,
		uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)), uintptr(syscall.MS_SYNC))
	if errno != 0 {
		return errno
	}
	return nil
}

// Float32ToFloat16 — conversion IEEE 754 half, round-half-to-even, identique au
// moteur horosvec (arena.go) pour parité de format.
func Float32ToFloat16(f float32) uint16 {
	b := math.Float32bits(f)
	sign := uint16((b >> 16) & 0x8000)
	exp := int32((b>>23)&0xff) - 127 + 15
	mant := b & 0x7fffff
	if (b>>23)&0xff == 0xff {
		if mant != 0 {
			return sign | 0x7e00
		}
		return sign | 0x7c00
	}
	if exp >= 0x1f {
		return sign | 0x7c00
	}
	if exp <= 0 {
		if exp < -10 {
			return sign
		}
		mant |= 0x800000
		shift := uint32(14 - exp)
		half := mant >> shift
		rem := mant & ((1 << shift) - 1)
		halfway := uint32(1) << (shift - 1)
		if rem > halfway || (rem == halfway && (half&1) == 1) {
			half++
		}
		return sign | uint16(half)
	}
	half := sign | uint16(exp<<10) | uint16(mant>>13)
	rem := mant & 0x1fff
	if rem > 0x1000 || (rem == 0x1000 && (half&1) == 1) {
		half++
	}
	return half
}

// Float16ToFloat32 — décodage identique au moteur horosvec.
func Float16ToFloat32(h uint16) float32 {
	sign := uint32(h&0x8000) << 16
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x03ff)
	if exp == 0 {
		if mant == 0 {
			return math.Float32frombits(sign)
		}
		exp32 := uint32(127 - 15 + 1)
		for mant&0x0400 == 0 {
			mant <<= 1
			exp32--
		}
		mant &= 0x03ff
		return math.Float32frombits(sign | exp32<<23 | mant<<13)
	}
	if exp == 0x1f {
		if mant == 0 {
			return math.Float32frombits(sign | 0x7f800000)
		}
		return math.Float32frombits(sign | 0x7f800000 | mant<<13)
	}
	exp32 := exp - 15 + 127
	return math.Float32frombits(sign | exp32<<23 | mant<<13)
}
