# Veille état de l'art — deux goulots d'un moteur vectoriel Go pur (CGO off)

Cadence de recherche : juillet 2026. Chaque énoncé notable porte sa source (URL en fin de note).
Distinction tenue : FAIT SOURCÉ (mesure ou affirmation d'une source primaire) vs INFÉRÉ
(déduction de l'auditeur appliquée à la stack locale). Aucune modification de code.

---

## Nœud 1 — Contention d'un verrou de cache en lecture (`sync.RWMutex`)

### Constat sourcé

- **FAIT (S1).** Un banc récent comparant six conceptions de cache Go établit que
  `sync.RWMutex` cesse de progresser au-delà d'environ quatre cœurs : « The shared reader
  counter becomes the new contention point; it stops improving after ~4 cores. » À huit cœurs
  sur charge de lecture uniforme, le `RWMutex` fait **pire** qu'un `Mutex` simple (259 ns/op
  contre 168 ns/op). Le compteur de lecteurs partagé est lui-même une ligne de cache disputée
  atomiquement — exactement le mécanisme mesuré localement (le `RLock` contentionne au-delà d'un
  certain parallélisme). [S1]
- **FAIT (S1).** La partition du verrou renverse la courbe : une carte à **256 partitions**
  (une partition = un mutex) atteint 11,5 ns/op à huit cœurs contre 168 ns/op pour le mutex
  unique, un facteur d'accélération de 6,9× et « up to 8× faster than a single sync.Mutex at
  8 cores ». Le passage d'un verrou à 256 fait bondir le débit de 4,6 à 43 Mops/s. Le verdict
  du banc désigne l'approche shardée comme **solution par défaut**, robuste sur toutes les
  distributions de charge. [S1]
- **FAIT (S1).** La lecture sans verrou par copie-sur-écriture (`atomic.Pointer` + échange
  atomique) domine en lecture pure (7 à 11,5 ns/op) mais effondre l'écriture (jusqu'à ~46,5 ms/op
  sur charge équilibrée) : elle ne vaut « only if writes are rare and batched ».
- **FAIT (S2).** `atomic.Pointer[T]` publie un instantané immuable d'un seul échange : les
  lecteurs chargent le pointeur courant sans verrou et ne voient jamais d'état partiel ; le
  patron canonique pour une carte read-mostly est de republier une copie dans un
  `atomic.Pointer[map[K]V]` à chaque écriture. [S2]
- **FAIT (S3).** Les structures concurrentes `xsync` (v4, Go ≥ 1.24) reposent sur une table de
  hachage à buckets de la taille d'une ligne de cache (CLHT) : chaque bucket a son propre mutex
  d'écriture tandis que **les lectures sont entièrement sans verrou par chargements atomiques**.
  L'auteur d'`xsync` note toutefois qu'en **lecture pure**, `sync.Map` et `xsync.Map` sont « on
  par » — le gain d'`xsync` porte surtout sur l'écriture concurrente et le sharding. [S3][S4]

### Maturité / récence

Consensus établi et daté 2025-2026 : le sharding de verrou et l'immuabilité + échange atomique
sont les deux remèdes documentés. Le banc S1 et l'article `xsync` S3 sont de la cadence 2025-2026 ;
le patron `atomic.Pointer` COW est stabilisé dans la doctrine d'optimisation Go S2.

### Applicabilité stack Go pure CGO-off

Intégrale. `sync`, `sync/atomic`, le sharding par tranche de hachage et `xsync` (Go pur, aucune
dépendance cgo) fonctionnent sous `CGO_ENABLED=0`. Aucun obstacle.

### Verdict Nœud 1

L'état de l'art **confirme** le remède retenu. Pour un cache read-mostly acquis ~128 fois par
opération sous ~32 goroutines, deux voies consensuelles : (a) **partitionner** le verrou (une
partition = un mutex, viser 256 partitions) — solution par défaut robuste en lecture ET écriture ;
(b) si les écritures sont rares et groupables, **lecture sans verrou** par `atomic.Pointer` sur un
instantané immuable. Le simple `RWMutex` unique est précisément l'anti-patron mesuré.

---

## Nœud 2 — Lectures bornées par la bande passante mémoire (rerank ANN)

### Constat sourcé

- **FAIT (S5).** Une étude d'ingénierie datée du 3 février 2026 sur un moteur ANN de production
  qualifie explicitement la recherche vectorielle de **« bandwidth-bound »** : « One field is as
  big as `DIM x DTYPE_SIZE` bytes, instead of a single `INT64`, and the entirety of the field is
  needed in the query hot path. » Elle chiffre le régime : à fp16, une lecture séquentielle de
  120 Go/s donne `120GB/(1x100MB) = 1,200 qps` — le débit est bien plafonné par la bande passante,
  pas par le calcul. [S5]
- **FAIT (S6).** La littérature ANN 2026 reconnaît que « modern ANNS engines rely on a costly
  second-pass refinement stage that reads full-precision vectors [...] and for modern embeddings,
  these reads now dominate query latency ». Le re-classement pleine précision est donc identifié
  comme le poste dominant de latence, memory-bandwidth-bound. [S6]
- **FAIT (S6/S7).** La quantification est le levier admis : fp16 (moitié de la bande passante de
  fp32), int8, puis quantifications à 1 bit (RaBitQ). L'étape de scan « IVF-Flat's scan stage is
  often dominated by memory bandwidth due to high-dimensional floating-point loads » ; l'intensité
  arithmétique de fp16 reste faible (0,125 op/octet à 128 dimensions), signature d'un régime borné
  mémoire, pas calcul. [S7]
- **FAIT (S5).** Sur le compromis rappel/bande passante : réduire la taille du vecteur (`f16 → bit`,
  facteur 1/16) rend l'étape « completely compute-bound » — la quantification déplace le goulot de
  la mémoire vers le calcul, preuve directe que le layout/précision gouverne le régime. L'auteur
  note qu'`INT8` calcule plus vite que fp16 pour la distance ; le patron admis est un premier tri
  sur vecteurs quantifiés puis un re-classement pleine précision **restreint au petit sommet des
  candidats**. [S5][S6]
- **FAIT (S6).** La disposition contiguë et le placement mémoire sont traités explicitement : les
  systèmes récents (HAVEN, FaTRQ, arXiv 2026) attaquent « capacity and data-movement bottlenecks »
  du rerank IVF-PQ, et l'accès dispersé de type graphe force les index à résider entièrement en
  DRAM. La contiguïté et l'alignement réduisent les transferts de lignes de cache — même principe
  que les buckets alignés ligne-de-cache du Nœud 1. [S5][S6]

### Maturité / récence

Sujet actif et convergent en 2024-2026 : arXiv (HAVEN, FaTRQ, IVF-TQ, RaBitQ), retours
d'ingénierie de production (S5, fév. 2026), et fonctions livrées (OpenSearch/FAISS fp16, int8).
La reconnaissance du rerank comme memory-bandwidth-bound est désormais un acquis, non une hypothèse.

### Applicabilité stack Go pure CGO-off

Le principe — layout contigu, demi-précision, re-classement restreint — est **agnostique au
langage** et implémentable en Go pur : `[]uint16` contigu pour fp16, alignement sur ligne de cache,
lecture en flux séquentiel. Réserve honnête : le SIMD explicite (AVX) et le prefetch logiciel des
sources citées supposent des intrinsèques indisponibles en Go pur sans assembleur (`.s`) ni cgo ;
en Go pur on capte le gain principal (bande passante halvée par fp16 + contiguïté), le compilateur
auto-vectorisant peu. La conversion fp16→fp32 pour le produit scalaire reste à la charge du Go pur.

### Verdict Nœud 2

L'état de l'art **confirme** le remède retenu. Relire par requête 128 vecteurs fp32 (512 octets)
dispersés dans le tas est le patron memory-bandwidth-bound canonique ; le passer en **fp16 contigu**
(256 octets, moitié de la bande passante) est exactement la première mitigation documentée. Gain
principal capté en Go pur ; l'accélération SIMD/prefetch/NUMA des papiers est un palier supérieur
requérant de l'assembleur, hors du strict Go pur.

---

## Synthèse des deux verdicts

Les deux remèdes retenus par l'équipe sont corroborés par l'état de l'art 2025-2026 :
- **Verrou** : cache shardé (défaut robuste) ou lecture lock-free `atomic.Pointer` (si écritures
  rares) — le `RWMutex` unique est l'anti-patron mesuré et documenté.
- **Bande passante** : layout contigu fp16 — halving de bande passante reconnu comme première
  mitigation d'un rerank memory-bandwidth-bound.

Les deux goulots partagent une racine physique commune : une ressource matérielle unique partagée
(le compteur de lecteurs / une ligne de cache pour le verrou ; la bande passante DRAM pour le
rerank) que le parallélisme sature. La partition (verrou) et la compaction (données) sont les deux
réponses structurellement analogues.

---

## Sources

- [S1] Shard your locks: benchmarking 6 Go cache designs — strebkov.dev : https://strebkov.dev/posts/shard-your-locks/
- [S2] Immutable Data Sharing / Atomic Operations — Go Optimization Guide : https://goperf.dev/01-common-patterns/immutable-data/ et https://goperf.dev/01-common-patterns/atomic-ops/
- [S3] So long, sync.Map — puzpuzpuz.dev : https://puzpuzpuz.dev/so-long-syncmap
- [S4] xsync — Concurrent data structures for Go (v4) : https://github.com/puzpuzpuz/xsync
- [S5] Case Study: turbopuffer ANN v3 (2026-02-03) — Terence Z. Liu : https://terencezl.github.io/blog/2026/02/03/case-study-turbopuffer-ann-v3/
- [S6] FaTRQ: Tiered Residual Quantization for LLM Vector Search (arXiv 2601.09985, jan. 2026) : https://arxiv.org/pdf/2601.09985 ; HAVEN (arXiv 2603.01175) : https://arxiv.org/pdf/2603.01175
- [S7] IVF-TQ (arXiv 2605.17415) et pratiques de quantification fp16/int8 (OpenSearch/FAISS) : https://arxiv.org/pdf/2605.17415
