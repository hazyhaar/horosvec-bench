# Arène croissante — magasin de vecteurs segmenté append-only (R&D, décision étayée)

Date : 2026-07-10. Ancrage : proj_horosvec_incremental. Chantier de faisabilité :
conception + prototype jetable + micro-banc. Lecture seule sur le moteur
`/devhoros/horosvec` (zéro modification). Prototype :
`/devhoros/horosvec-bench/pkg/growarena/` + `/devhoros/horosvec-bench/cmd/proto-growarena/`.

## 1. Le problème

Le moteur horosvec dispose de deux régimes de stockage des vecteurs de re-classement,
et aucun ne cumule les deux propriétés requises (FAITS SOURCÉS, mesures de la campagne
`2026-07-10_bench_arene_vs_sqlite.md`, non re-mesurées ici) :

- **Arène mmap fp16** (`HVARENA1`) : lecture concurrente sans verrou, ~72 kQPS de
  recherche de bout en bout à 32 clients — mais FIGÉE : `Insert` refuse fail-loud tout
  index arène (`horosvec.go:1090`), la couverture 0..count-1 est gravée au build.
- **SQLite direct** (modernc, pur Go) : inscriptible — mais plafonné à ~37-40 kQPS,
  coude net entre 8 et 16 clients, sérialisation interne de `modernc.org/sqlite` non
  levée par un pool dimensionné (+8 % seulement).

Le verrou technique : faire CROÎTRE un mmap invalide les pointeurs des lecteurs en vol
(un `mremap`/re-`mmap` d'une zone vivante expose à la lecture déchirée et au SIGSEGV).

## 2. Design retenu — magasin segmenté append-only (patron Lucene/LSM)

```
   node_id dense ────────────┐
                             ▼
   seg = id / segCap    off = header + (id % segCap) · dim · 2
                             │
  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────────┐
  │ seg-000000   │  │ seg-000001   │  │ seg-000002 (queue)       │
  │ SCELLÉ       │  │ SCELLÉ       │  │ pré-alloué PLEIN, mmap RW│
  │ mmap 1 fois  │  │ mmap 1 fois  │  │ 1 fois, JAMAIS remappé   │
  └──────────────┘  └──────────────┘  │ écrivain unique → append │
                                      └──────────────────────────┘
  table des segments : atomic.Pointer[[]*segment]  (swap à l'ajout d'un segment)
  visibilité        : atomic.Int64 count           (release-store après les octets)
```

Trois décisions structurantes, qui ensemble dissolvent le verrou :

1. **Aucune zone mmap ne change jamais de taille.** Chaque segment est créé
   pré-alloué à sa capacité PLEINE (`ftruncate` puis un unique `mmap MAP_SHARED`),
   y compris le segment de queue. La croissance du magasin n'est pas la croissance
   d'un mapping : c'est l'AJOUT d'un fichier neuf. Les pointeurs des lecteurs en vol
   ne sont donc jamais invalidés — le danger du remap est éliminé par construction,
   pas mitigé.
2. **La visibilité est un compteur atomique, pas la taille du fichier.** L'écrivain
   copie les octets fp16 du vecteur dans la zone de queue, PUIS publie
   `count.Store(n+1)` (release). Un lecteur fait `count.Load()` (acquire) et refuse
   tout `node_id >= count`. Le happens-before de `sync/atomic` garantit qu'un vecteur
   visible est un vecteur complet : la lecture déchirée est impossible tant que le
   magasin est append-only (les octets publiés ne sont jamais réécrits).
3. **La table des segments est publiée par swap de pointeur.** L'écrivain étend une
   COPIE du slice et la publie par `atomic.Pointer.Store` ; un lecteur qui détient
   l'ancienne vue reste correct (ses segments existent toujours ; les nouveaux ids
   lui sont simplement invisibles jusqu'à sa prochaine acquisition). Lecture 100 %
   lock-free : deux loads atomiques + arithmétique d'offset.

### Alternatives écartées

| Voie | Verdict |
|---|---|
| Fichier unique sur-provisionné + `ftruncate` | Écarté : impose de figer une capacité max au départ (ou de remapper au dépassement — le danger revient) ; l'espace adressé s'engage d'un bloc. Le segmenté paie ~1 mmap par segment, coût nul mesuré. |
| Copy-on-grow (recopier dans une arène plus grande, swap) | Écarté : coût O(N) par croissance, double empreinte transitoire (2×27 Go à l'échelle réelle), et l'ancienne copie ne peut être unmappée sous les lecteurs en vol sans comptage de références — la complexité exacte que le segmenté évite. |
| Double-buffer | Même objection : double empreinte + le problème de retrait sûr du buffer retiré (hazard pointers / epochs, lourd en Go sans GC de mmap). |

### Points durs tranchés

- **Granularité de segment** : 65 536 vecteurs par segment (à dim 128 fp16 : 16 Mo).
  À 26,7 M de vecteurs : ~408 segments, ~408 mmaps — très en deçà de
  `vm.max_map_count` (défaut 65 530). Le banc montre un surcoût de segmentation nul
  (§4). Paramètre libre du design, à recalibrer par dimension.
- **Durabilité** : `Flush()` = `msync(MS_SYNC)` des segments sales puis gravure du
  compteur `committed` dans le header du segment + msync de la page header. À la
  réouverture, seuls les vecteurs ≤ committed sont visibles ; un append non flushé
  avant crash est perdu — même contrat qu'un WAL non commité, pas de demi-vecteur
  possible (le compteur n'avance qu'après les octets). Cadence de flush = politique
  de l'appelant (au banc : tous les 10 000 appends).
- **Réconciliation node_id dense** : le magasin est keyé par le même node_id dense
  0..n-1 que le graphe Vamana — l'append attribue `id = count`, exactement la
  discipline d'`Insert` du moteur. Ordre d'écriture au câblage futur : vecteur dans
  l'arène croissante d'abord, commit SQLite du nœud ensuite ; un vecteur orphelin en
  queue d'arène (crash entre les deux) est écrasable au prochain append, jamais
  l'inverse (un nœud du graphe sans vecteur serait, lui, une corruption).
- **Le graphe (voisins) reste en SQLite/plan chaud.** Ce sous-projet ne co-segmente
  pas l'adjacence : `Insert` du moteur sait déjà écrire le graphe en SQLite ; seul le
  refus arène (`horosvec.go:1090`) tomberait. Le plan chaud reste la voie de lecture
  des voisins.
- **Compaction** : non nécessaire au régime append-only pur (pas de tombstone, pas de
  fragmentation interne — segments pleins à 100 % sauf la queue). Une suppression
  logique relèverait du graphe, pas du magasin de vecteurs. Hors périmètre, assumé.
- **Unmap des segments** : JAMAIS en vie de process (seul `Close`, après arrêt des
  lecteurs). C'est le point dur nommé honnêtement : un retrait de segment à chaud
  exigerait un schéma epoch/refcount. Le design l'esquive en ne retirant rien — coût
  accepté : l'espace adressé ne fait que croître, arbitré par le page cache noyau
  (adresses virtuelles, pas de la RAM).

## 3. Protocole du micro-banc

Prototype pur stdlib (`GOWORK=off CGO_ENABLED=0`, modernc uniquement pour le magasin
de référence SQLite). Machine : 32 cœurs, NVMe, cache chaud. Trois magasins mesurés à
ISOPÉRIMÈTRE (même opération : lecture d'UN vecteur par node_id aléatoire + décodage
fp16→fp32 complet, conversions identiques à `arena.go` du moteur) :

- `proto-segmented` : growarena, segCap 65 536 (4 segments à 200k, 16 à 1M) ;
- `flat-mmap-arena-like` : growarena monosegement (un seul mmap ≈ l'arène figée) ;
- `sqlite-modernc-blob` : `SELECT vec FROM vecs WHERE id=?` préparée, pool 256, WAL,
  mmap_size 256 Mo.

Sweep 1→128 goroutines, fenêtres ≥ 1 s, ids aléatoires, motif de contenu vérifié à
chaque lecture (échec = panic). Commandes :

```
env GOWORK=off CGO_ENABLED=0 go build -o proto-growarena ./cmd/proto-growarena
./proto-growarena -n 200000  -dim 128 -dur 1.5 -verif 10
./proto-growarena -n 1000000 -dim 128 -dur 1.0 -verif 5
```

NOTE de comparabilité : les références de la mission (arène ~72 kQPS, modernc
~37-40 kQPS) sont des débits de RECHERCHE de bout en bout (marche de graphe + re-rank
~100+ lectures de vecteurs par requête). Le présent banc mesure la primitive « une
lecture de vecteur » — les chiffres sont donc en MILLIONS d'ops/s et se comparent
entre eux ; le lien aux références se fait par le RAPPORT proto/flat (le flat étant
le régime de l'arène qui donne les 72 k) et par la forme des courbes (le coude modernc
réapparaît à l'identique).

## 4. Résultats mesurés (sorties collées)

### 200 000 vecteurs, dim 128 (lectures de vecteur par seconde)

| Conc. | proto segmenté | flat (≈arène) | sqlite modernc | proto/flat | proto/sqlite |
|---|---|---|---|---|---|
| 1   | 3 365 450  | 3 278 745  | 323 267 | 1.03× | 10.4× |
| 2   | 6 837 477  | 6 815 864  | 525 389 | 1.00× | 13.0× |
| 4   | 13 360 693 | 12 995 117 | 754 367 | 1.03× | 17.7× |
| 8   | 24 616 130 | 23 401 895 | 671 707 | 1.05× | 36.6× |
| 16  | 36 781 862 | 39 869 817 | 677 800 | 0.92× | 54.3× |
| 32  | 59 264 141 | 54 814 135 | 595 631 | 1.08× | 99.5× |
| 64  | 58 976 589 | 58 575 614 | 555 836 | 1.01× | 106×  |
| 128 | 52 033 779 | 57 918 182 | 604 071 | 0.90× | 86×   |

### 1 000 000 vecteurs, dim 128 (16 segments)

| Conc. | proto segmenté | flat (≈arène) | sqlite modernc | proto/flat |
|---|---|---|---|---|
| 1   | 2 998 296  | 3 015 667  | 310 730 | 0.99× |
| 8   | 20 444 120 | 19 247 971 | 718 561 | 1.06× |
| 32  | 44 022 063 | 41 097 795 | 502 829 | 1.07× |
| 128 | 51 236 785 | 51 445 453 | 587 788 | 1.00× |

Lectures : (INFÉRÉ des chiffres ci-dessus) le proto segmenté est INDISCERNABLE du
mmap plat (rapport 0.90–1.08×, bruit de mesure) — le surcoût de la segmentation
(division, deux loads atomiques, indirection de table) est nul en pratique. La
signature modernc de la campagne se retrouve à l'identique : montée jusqu'à ~4
clients puis plateau bas (~0,6-0,75 M lect./s), pendant que le mmap monte quasi
linéairement jusqu'à la saturation des cœurs. Sur la primitive isolée, l'écart
atteint ~100× à 32 clients (au niveau recherche complète il se dilue en ~2×,
cf. les 72 k vs 37-40 k de référence — le SQL n'est qu'une part du coût par requête).

### Preuve de l'append-concurrent-sans-casse (sorties collées)

```
run 200k : {"test":"concurrent_append","readers":32,"appends":4485060,"reads":289756605,"torn_or_wrong":0,"final_count":4485060}
           {"test":"reopen","count_before_close":4485060,"count_after_reopen":4485060,"pattern_mismatches":0}
run 1M   : {"test":"concurrent_append","readers":32,"appends":2269635,"reads":145445651,"torn_or_wrong":0,"final_count":2269635}
           {"test":"reopen","count_before_close":2269635,"count_after_reopen":2269635,"pattern_mismatches":0}
```

Un écrivain unique a appendé 4,49 M de vecteurs en 10 s (~448 k appends/s, 69
segments créés à chaud) pendant que 32 lecteurs vérifiaient le motif de CHAQUE
vecteur lu : 289,8 M de lectures vérifiées, **0 lecture déchirée, 0 panic, 0 valeur
fausse**. Après Close et réouverture, le compte et l'intégralité du motif sont
intacts. L'append-concurrent tient au sol : **oui**.

Limite d'honnêteté : le détecteur de course Go (`-race`) n'a pas été exécuté (CGO
off, doctrine workspace) ; la preuve est comportementale (motif vérifié sur 435 M de
lectures cumulées sous churn d'appends et de créations de segments), et l'argument de
correction est le happens-before release/acquire de `sync/atomic`, standard Go.

## 5. Verdict coût/bénéfice

**Arène croissante viable — et le chantier de câblage se justifie, mais comme chantier
BORNÉ, pas urgent.**

- **Faisabilité : acquise.** Pur stdlib, CGO off, zéro dépendance. Le débit de
  lecture concurrente égale l'arène figée (1.0× ±8 %) ; l'append en ligne (~450 k/s
  au banc, soit 40 s pour absorber les 11 k inserts/jour réels… par seconde de
  marge) ne perturbe pas les lecteurs. Le verrou « croissance mmap » est dissous par
  construction (aucun remap), pas contourné.
- **Coût de complexité : maîtrisé.** ~300 lignes de magasin, un seul invariant
  mémoire (octets avant compteur), pas d'epoch/hazard-pointer (rien n'est retiré à
  chaud), durabilité par compteur committed. Le câblage moteur réel ajouterait :
  lever le refus `horosvec.go:1090`, router `Insert` vers l'append d'arène, étendre
  le plan chaud incrémentalement (le vrai morceau restant, HORS périmètre prouvé ici).
- **Face au repli « rebuild nocturne »** : à 11 k inserts/jour, le rebuild nocturne
  reste SUFFISANT pour la fraîcheur J+1 — l'arène croissante ne se justifie QUE si
  une fraîcheur infra-journalière (un document interrogeable minutes après son
  ingestion) est une exigence produit. La présente R&D établit que cette exigence,
  si elle est posée, est atteignable sans sacrifier le débit ni la pureté CGO-off ;
  elle n'établit pas que l'exigence existe. Recommandation : graver le design,
  différer le câblage jusqu'à ce qu'un consommateur réclame l'infra-journalier ;
  le jour venu, le chemin est prouvé et chiffré.

CLAIM:: l'arène croissante horosvec est un magasin segmenté append-only : des segments mmap pré-alloués pleins et jamais remappés, une visibilité par compteur atomique release/acquire, une table de segments publiée par swap de pointeur — la croissance ajoute des fichiers, elle ne retaille jamais un mapping.
CLAIM-NEG:: l'arène croissante n'est pas un remplacement du SQLite horosvec — le graphe, les codes RaBitQ et la transactionnalité restent en base ; elle ne remplace que le fichier sidecar fp16 figé.
