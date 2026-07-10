# Banc SIMD pur-Go (`simd/archsimd`) vs scalaire — noyaux de distance horosvec

Date : 2026-07-10. Zone : `/devhoros/horosvec-bench/kernels/simdbench` (bench jetable,
module `github.com/hazyhaar/horosvec-bench`). Lecture seule sur `/devhoros/horosvec` —
aucune ligne du moteur modifiée. Toolchain : `go1.26.3`, `GOEXPERIMENT=simd`,
`CGO_ENABLED=0`, machine Intel i9-14900K.

## 1. Nature du noyau de la marche greedy (~70 % CPU)

Source : `/devhoros/horosvec/horosvec.go:1006-1052` (`rabitqGreedySearchInternal`) appelle,
pour la distance du médoïde puis pour CHAQUE voisin visité pendant le faisceau, la fonction
`rabitqDistanceLUT` (`/devhoros/horosvec/rabitq.go:140-151`) :

```go
var signDot float64
for b, code := range storedCode {
    signDot += lut[b*256+int(code)]
}
```

C'est une **accumulation de table (LUT/ADC)** : pour chaque octet du code RaBitQ stocké
(1 bit par dimension, 8 dimensions par octet), l'index `code` (0-255) sélectionne une entrée
précalculée dans une table de 256 `float64` par position d'octet (`buildRabitqLUT`,
`rabitq.go:110-137`), et les 64 valeurs lues (dim=512 → 64 octets) sont sommées. Ce n'est
**ni** un produit scalaire fp classique **ni** un simple popcount/XOR de codes — c'est un
**gather mémoire indexé par la donnée** (l'octet du code choisit l'adresse à lire), suivi
d'une réduction additive. C'est la réponse à la question posée par la mission : le noyau
dominant de la marche est (a) LUT/ADC (gather + add), pas (b) popcount/XOR ni (c) un produit
scalaire fp.

Le noyau symétrique par popcount (`rabitqDistance`, `rabitq.go:161-203`, XOR + `bits.OnesCount64`)
existe dans le code mais est explicitement documenté comme **hors chemin de production**
(« cette variante symétrique... n'existe que pour le banc de comparaison, jamais sur le
chemin chaud », commentaire `rabitq.go:156-160`) : la voie réelle est asymétrique via LUT.

## 2. Nature du noyau du rerank (~2 % CPU)

Source : `/devhoros/horosvec/vamana.go:75-92` (`l2DistanceSquared`), consommé au rerank
exact des candidats finaux. C'est une **soustraction-carré-somme fp32→fp64 lane-wise**,
triviale à vectoriser (accès mémoire contigu, aucune indirection).

## 3. Ce qu'expose `simd/archsimd` (sondé au doc, Go 1.26.3)

`GOEXPERIMENT=simd go doc -all simd/archsimd` confirme la compilation sous le flag et
expose des types vectoriels (`Float32x8/16`, `Int8x…`, etc.) avec Add/Sub/Mul/And/Xor/
Permute/ConcatPermute/Compress/Expand. **Sondé et confirmé absent** :

- Aucune méthode `PopCount` sur aucun type entier (`grep -i popcount` sur le doc complet :
  0 résultat).
- Aucune opération `Gather` indexée par un vecteur entier sur de la mémoire arbitraire
  (`grep -i gather` : 0 résultat). Les seules opérations de réarrangement sont `Permute` /
  `ConcatPermute`, qui opèrent **en registre** sur les lanes du vecteur lui-même (8, 16, 32,
  64 lanes selon le type), pas un gather mémoire général sur une table de 256 entrées
  adressée par un octet arbitraire.

**Conséquence directe** : le noyau réel de la marche (LUT/ADC, gather sur table 256×float64
indexée par octet) n'est **pas exprimable** avec les primitives actuelles de
`simd/archsimd`. Même le noyau symétrique alternatif (XOR + popcount) — qui pourrait en
théorie être vectorisé si le paquet exposait un popcount vectoriel — reste hors de portée
faute de cette primitive.

## 4. Mesures

Bench Go natif, dim=512, `benchtime=200000x`, moyenne stable sur trois passes rapprochées.

| Noyau | Rôle | Scalaire (ns/op) | SIMD (ns/op) | Facteur |
|---|---|---:|---:|---:|
| `l2DistanceSquared` (rerank) | ~2 % CPU | 300.3 – 357.4 | 124.0 | **~2,4-2,9×** |
| `rabitqDistanceLUT` (marche greedy) | ~70 % CPU | 19.0 – 20.6 | **non exprimable** | — |
| `rabitqDistance` XOR+popcount (alternatif, hors prod) | — (référence) | 118.2 – 599.0 (bruit petit N) | **non exprimable** (pas de popcount vectoriel) | — |

Sorties brutes collées (build scalaire sans flag, confirme le fallback) :

```
$ env GOWORK=off CGO_ENABLED=0 go test -run=^$ -bench=. -benchtime=200000x ./...
BenchmarkL2DistanceSquaredScalar-32      200000    357.4 ns/op
BenchmarkRabitqDistanceLUTScalar-32      200000     20.57 ns/op
BenchmarkRabitqXORPopcountScalar-32      200000    135.2 ns/op
```

Sorties brutes collées (build SIMD, `GOEXPERIMENT=simd`) :

```
$ env GOEXPERIMENT=simd GOWORK=off CGO_ENABLED=0 go test -run=^$ -bench=. -benchtime=200000x ./...
BenchmarkL2DistanceSquaredSIMD-32        200000    124.0 ns/op
BenchmarkL2DistanceSquaredScalar-32      200000    300.3 ns/op
BenchmarkRabitqDistanceLUTScalar-32      200000     19.02 ns/op
BenchmarkRabitqXORPopcountScalar-32      200000    118.2 ns/op
```

Correction lane-par-lane vérifiée par `TestL2DistanceSquaredSIMDMatchesScalar` (20 tirages
aléatoires, tolérance 1e-3) : `PASS`.

Fallback sans flag confirmé : `go build ./...` et `go test ./...` (sans `GOEXPERIMENT=simd`)
compilent et passent, le fichier `kernels_simd.go` porte le tag de build
`//go:build goexperiment.simd` et est simplement exclu de la compilation par défaut.

## 5. Verdict

Le SIMD pur-Go (`simd/archsimd`) accélère bien le **noyau du rerank** (facteur ~2,4-2,9× sur
`l2DistanceSquared`), mais celui-ci ne pèse que ~2 % du CPU de recherche. Il **ne peut pas**
accélérer le **noyau de la marche greedy** (~70 % du CPU) : ce noyau est un gather de table
indexé par octet (LUT/ADC), pas un produit lane-wise ni un popcount, et `simd/archsimd`
n'expose ni gather mémoire général ni popcount vectoriel à ce jour (Go 1.26.3). Le SIMD pur-Go
**frappe le mauvais goulot** — il traite la partie qui compte le moins et laisse intact le
noyau dominant.

## 6. Caveats obligatoires

(a) `simd/archsimd` est **expérimental**, hors promesse de compatibilité Go 1, n'existe que
sous `GOEXPERIMENT=simd` : l'embarquer dans un moteur servi est un choix d'architecture à
peser (toolchain de build spécialisée, portabilité restreinte à AMD64 documentée par le
paquet lui-même), indépendamment du présent constat de performance.

(b) L'accélération sur `l2DistanceSquared` (rerank, ~2 % CPU) **ne se transfère pas** au
noyau de la marche (~70 % CPU) — les deux noyaux sont de nature différente (lane-wise fp32
contigu vs gather LUT indexé par octet). Présenter le facteur ~2,4-2,9× comme un gain
applicable à la recherche globale serait trompeur : appliqué uniquement au rerank, l'effet
net sur le temps de recherche total resterait de l'ordre de quelques points de pourcent au
mieux (rerank ≈ 2 % du CPU), tandis que le goulot dominant (marche greedy) reste scalaire.

## 7. Piste non explorée (hors périmètre de ce banc)

Un noyau LUT/ADC de ce type se vectorise usuellement par une technique de type
« fastscan » (SIMD-PQ, cf. FAISS) : découper la table 256 entrées en 2×16 (nibbles) et
utiliser une instruction de **shuffle intra-registre** (16 entrées, adressable en un seul
`Permute`/`Shuffle` sur un registre 128 bits) au lieu d'un gather 256 entrées. Cette
restructuration change la représentation de la LUT et du code (16 entrées par nibble au lieu
de 256 par octet) — c'est une réécriture structurelle du format RaBitQ/LUT, hors périmètre
de ce banc de mesure (qui compare les noyaux existants tels quels) et hors mandat (lecture
seule sur le moteur). Signalé comme piste d'ingénierie future, non chiffré ici.
