// Package storagemedium résout le support de stockage physique derrière un chemin
// de système de fichiers : disque rotationnel, SSD, ou indéterminé. Il sert à NOMMER
// le support dans chaque point de mesure du banc, après l'incident 2026-07 (deux jours
// de mesures rendues inexploitables faute de savoir si l'arène vivait sur du rotationnel,
// où la latence mesurée dépasse le SSD d'un facteur 100 à 370).
//
// La résolution est FAIL-SOFT par construction : toute impossibilité (chemin inexistant,
// pseudo-système de fichiers sans device bloc, attribut absent) rend "unknown" sans jamais
// remonter d'erreur bloquante. Un banc ne doit jamais échouer parce que son support est
// indéterminable ; il doit seulement le dire honnêtement.
//
// Chemin de résolution (Linux) : stat(2) du chemin donne le device (major:minor) portant
// l'inode ; /sys/dev/block/<major>:<minor> pointe le nœud bloc correspondant. L'attribut
// queue/rotational se lit sur le disque entier — pour une partition (le device stat pointe
// souvent une partition), on remonte le lien réel jusqu'au répertoire portant queue/rotational.
// Ce chemin donne une réponse agrégée honnête pour les devices empilés (RAID md, LVM dm),
// qui synthétisent eux-mêmes leur propre queue/rotational ; à défaut de pouvoir la lire,
// "unknown" est rendu plutôt qu'une valeur inventée.
package storagemedium

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Medium énumère les supports reconnus.
const (
	Rotational = "rotational"
	SSD        = "ssd"
	Unknown    = "unknown"
)

// Info porte le verdict de résolution. Device est le nom du disque sous /sys/block
// effectivement interrogé (ex. "nvme0n1", "sda", "dm-0"), vide si non résolu.
type Info struct {
	Medium string
	Device string
}

// Resolve retourne le support de stockage du chemin donné, fail-soft. En cas
// d'impossibilité (chemin absent, pas de device bloc, attribut illisible), rend
// {Medium: Unknown, Device: ""} sans erreur.
func Resolve(path string) Info {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return Info{Medium: Unknown}
	}
	dev := uint64(st.Dev)
	maj := major(dev)
	min := minor(dev)
	if maj == 0 {
		// Pseudo-FS (tmpfs, overlay sans device bloc) : major 0 → aucun disque physique.
		return Info{Medium: Unknown}
	}

	sysLink := filepath.Join("/sys/dev/block", itoa(maj)+":"+itoa(min))
	real, err := filepath.EvalSymlinks(sysLink)
	if err != nil {
		return Info{Medium: Unknown}
	}

	// Remonter jusqu'au répertoire portant queue/rotational (le disque, pas la partition).
	dir := real
	for {
		rot := filepath.Join(dir, "queue", "rotational")
		if b, rerr := os.ReadFile(rot); rerr == nil {
			return Info{Medium: classify(b), Device: filepath.Base(dir)}
		}
		parent := filepath.Dir(dir)
		if parent == dir || parent == "/" || parent == "/sys" || !strings.HasPrefix(parent, "/sys") {
			return Info{Medium: Unknown}
		}
		dir = parent
	}
}

// classify interprète le contenu de queue/rotational : "1" → rotationnel, "0" → SSD.
func classify(b []byte) string {
	switch strings.TrimSpace(string(b)) {
	case "1":
		return Rotational
	case "0":
		return SSD
	default:
		return Unknown
	}
}

// major/minor décodent un dev_t Linux selon l'encodage glibc (gnu_dev_major/minor),
// celui que porte syscall.Stat_t.Dev. Dépendance-libre (pas de golang.org/x/sys).
func major(dev uint64) uint64 {
	return (dev>>8)&0xfff | (dev>>32)&^uint64(0xfff)
}

func minor(dev uint64) uint64 {
	return dev&0xff | (dev>>12)&^uint64(0xff)
}

// itoa formate un uint64 décimal sans passer par strconv (garde le paquet minimal).
func itoa(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
