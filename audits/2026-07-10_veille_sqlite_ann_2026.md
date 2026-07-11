# Veille état de l'art — SQLite concurrent + ANN incrémental (cadence juin-juillet 2026)

Note de renseignement technique destinée à l'architecture du moteur ANN pur-Go
(graphe Vamana + quantification RaBitQ 1-bit + arène mmap fp16, base
`modernc.org/sqlite`, `CGO_ENABLED=0`). Chaque affirmation notable porte son
URL. La distinction entre **FAIT SOURCÉ** (oracle décidable : doc officielle,
release note, dépôt, papier, benchmark reproductible) et **INFÉRÉ** (déduction
d'ingénierie non étayée par une mesure directe sur notre stack) est explicite.

---

## NŒUD 1 — Concurrence de lecture SQLite sous forte charge parallèle

### Constats sourcés

- **WAL est le levier premier de la lecture concurrente.** En mode Write-Ahead
  Logging, les lecteurs ne bloquent pas les écrivains et réciproquement ; un
  nombre arbitraire de lectures peut progresser simultanément. Documentation
  officielle : <https://sqlite.org/wal.html>. FAIT SOURCÉ.

- **Pattern consensuel : deux pools séparés — un écrivain unique, N lecteurs
  read-only en WAL.** Le pool lecteur autorise le multiplexage de connexions
  (sûr en lecture seule), le pool écrivain est borné à une connexion. Voir la
  pratique décrite pour Go/`database/sql` :
  <https://dev.to/lovestaco/high-performance-sqlite-reads-in-a-go-server-4on3>
  et le driver `tailscale/sqlite` qui expose une fonction `ReadOnly` appliquant
  `PRAGMA query_only` par connexion :
  <https://pkg.go.dev/github.com/tailscale/sqlite>. FAIT SOURCÉ (pratique
  documentée), mais le dimensionnement optimal reste dépendant de la charge.

- **Pragmas de lecture haute performance** couramment recommandés :
  `journal_mode=WAL`, `synchronous=NORMAL`, `mmap_size` élevé (jusqu'à plusieurs
  Go), `cache_size` négatif (Kio), `temp_store=MEMORY`, `query_only=ON` sur les
  connexions lecture. Source :
  <https://dev.to/software_mvp-factory/sqlite-wal-mode-and-connection-strategies-for-high-throughput-mobile-apps-beyond-the-basics-eh0>.
  FAIT SOURCÉ (recommandation), l'effet réel dépend du profil d'accès.

- **`database/sql` : le pool doit être dimensionné explicitement.** Pour
  `modernc.org/sqlite`, `SetMaxOpenConns(>0)` est nécessaire pour exploiter la
  concurrence ; un pool réduit sérialise les goroutines. Source benchmark :
  <https://github.com/cvilsmeier/go-sqlite-bench> et
  <https://datastation.multiprocess.io/blog/2022-05-12-sqlite-in-go-with-and-without-cgo.html>.
  FAIT SOURCÉ.

- **`modernc.org/sqlite` (pur-Go) vs `mattn/go-sqlite3` (CGO) en lecture
  concurrente.** Les benchmarks montrent modernc légèrement en retrait en lecture
  concurrente pure (N=2/4/8 goroutines : 850/1190/2061 ops pour modernc contre
  948/1237/2395 pour mattn), soit ~0,85× à 8 clients ; mais modernc **dépasse**
  mattn sur de grandes requêtes SELECT (N=200k : 1094 vs 376 ops). L'écart
  global est modéré (« ~75 % de la vitesse CGO » à « entre 10 % plus lent et 2×
  plus lent selon l'opération »). Sources :
  <https://github.com/cvilsmeier/go-sqlite-bench>,
  <https://github.com/multiprocessio/sqlite-cgo-no-cgo>,
  <https://datastation.multiprocess.io/blog/2022-05-12-sqlite-in-go-with-and-without-cgo.html>.
  FAIT SOURCÉ (mais bancs génériques, non représentatifs d'un accès vectoriel).

### Évolutions de concurrence — maturité 2026

- **`BEGIN CONCURRENT`, WAL2, HC-tree restent sur des branches expérimentales,
  hors branche principale, non recommandés en production en 2026.** `BEGIN
  CONCURRENT` (branche `begin-concurrent`) autorise plusieurs écrivains à
  progresser en parallèle mais sérialise les COMMIT ; WAL2 (branche) élimine la
  croissance non bornée du WAL par deux fichiers ; HC-tree est un backend
  expérimental haute-concurrence à format de fichier distinct. Documentation
  officielle HC-tree/begin-concurrent :
  <https://sqlite.org/hctree/doc/begin-concurrent/doc/begin_concurrent.md>,
  <https://sqlite.org/hctree/doc/hctree/doc/hctree/index.html>. Constat de
  maturité (mai 2026, « pas dans la branche principale, probablement pas
  prêtes ») :
  <https://powersync.com/blog/sqlite-persistence-on-the-web>. FAIT SOURCÉ.
  Note : ces branches ciblent la concurrence en **écriture** ; le goulot décrit
  ici est en **lecture**, donc peu pertinentes pour notre cas.

### Applicabilité stack pur-Go / CGO-off & verdict

Le décrochage observé (débit ~0,5× vs mmap à 32 clients) est cohérent avec une
sérialisation par pool sous-dimensionné et/ou une contention dans
`database/sql` plutôt qu'avec une limite intrinsèque de SQLite en lecture WAL.
Le pattern consensuel « pool de connexions read-only, WAL, `mmap_size` élevé,
`query_only`, une connexion par goroutine active » est **la réponse
communément admise** et directement applicable à `modernc.org/sqlite`
(CGO-off préservé). Aucune des branches expérimentales n'est requise ni mûre.
**Verdict : câbler proprement le pool lecteur (WAL + N connexions read-only +
`mmap_size`) avant de conclure à une limite de SQLite ; l'arène mmap fp16 reste
supérieure pour le chemin chaud figé, SQLite servant le mutable.** La supériorité
mesurée de l'arène mmap sur SQLite dans le cas d'usage précis reste à confirmer
au sol après optimisation du pool (INFÉRÉ tant que le banc n'est pas rejoué).

---

## NŒUD 2 — Stockage vectoriel, quantification et paliers de cache

### Constats sourcés

- **RaBitQ (2024) : quantification 1-bit à borne d'erreur théorique, sans
  entraînement de codebook.** Estimateur de distance non biaisé, 32× de
  compression (1 bit/dimension vs 32 bits FP32). Papier :
  <https://www.researchgate.net/publication/381022541_RaBitQ_Quantizing_High-Dimensional_Vectors_with_a_Theoretical_Error_Bound_for_Approximate_Nearest_Neighbor_Search>.
  FAIT SOURCÉ.

- **Rappel réel du 1-bit SEUL (sans re-classement pleine précision) : ~76 %
  sur le banc Milvus.** Point capital et **correcteur d'une confabulation** :
  les résumés agrégés annoncent « >94 % sans reranking », mais la source
  primaire Milvus mesure **76 % de rappel en 1-bit standalone**, jugé
  « insuffisant pour les applications exigeant une haute précision ». Le rappel
  ne remonte à **94,7 %** (proche du plancher IVF_FLAT à 95,2 %) **qu'avec
  raffinement SQ8**. Source primaire décidable :
  <https://milvus.io/blog/bring-vector-compression-to-the-extreme-how-milvus-serves-3%C3%97-more-queries-with-rabitq.md>
  (« refinement is essential for recovering accuracy »). FAIT SOURCÉ. Le chiffre
  « 99,3 % de rappel 1-bit » circulant ailleurs correspond à des configurations
  IVFRaBitQ spécifiques et **ne doit pas être pris pour le rappel 1-bit-seul
  générique** — INFÉRÉ/contexte-dépendant, à ne pas généraliser.

- **Extended RaBitQ (2024) : quantification scalaire à taux arbitraire.** 4-bit
  ~90 %, 5-bit ~95 %, 7-bit ~99 % de rappel, tous sans reranking ; asymptotique-
  ment optimal. Sources :
  <https://dev.to/gaoj0017/extended-rabitq-an-optimized-scalar-quantization-method-83m>,
  <https://milvus.io/blog/turboquant-rabitq-vector-database-cost.md>. FAIT
  SOURCÉ. Implication : un palier intermédiaire (4-5 bit) offre un compromis
  rappel/mémoire nettement supérieur au 1-bit-seul pour un surcoût modéré.

- **`sqlite-vec` : PAS d'indexation ANN implémentée début 2026 — recherche
  brute-force uniquement.** Écrit en C pur, sans dépendance, tables virtuelles
  `vec0`, vecteurs float/int8/binaire. Benchmarks publiés jusqu'à ~1 M de
  vecteurs ; dégradation attendue à 10 M faute d'index. Sources :
  <https://marcobambini.substack.com/p/the-state-of-vector-search-in-sqlite>,
  <https://grokipedia.com/page/Comparison_of_sqlite-vec_and_pgvector>. FAIT
  SOURCÉ. Note stack : `sqlite-vec` est en **C** → incompatible avec
  `CGO_ENABLED=0`, donc **hors périmètre** pour notre binaire pur-Go de toute
  façon.

### Applicabilité & verdict

L'architecture à trois paliers (arène mmap fp16 figée 27 Go / blob SQLite mutable
/ codes 1-bit ~1,7 Go) est saine : les codes 1-bit servent au **filtrage
grossier** rapide, la pleine précision fp16 (mmap) au **re-classement** qui
restaure le rappel. La littérature confirme que **le 1-bit-seul ne suffit pas**
(~76 %) et que le re-classement pleine précision est la voie de restauration —
ce qui **valide** le maintien de l'arène fp16 plutôt que les seuls codes 1-bit.
`sqlite-vec` est écarté deux fois (pas d'ANN, exige CGO). **Verdict : conserver
l'arène fp16 comme palier de re-classement obligatoire derrière les codes 1-bit ;
envisager un palier Extended-RaBitQ 4-5 bit comme alternative au couple
1-bit+fp16 si l'empreinte mémoire de l'arène devient contraignante.**

---

## NŒUD 3 — Rafraîchissement incrémental d'un index ANN sans reconstruction

### Constats sourcés

- **FreshDiskANN / FreshVamana (2021, référence fondatrice toujours canonique) :
  pattern deux-tiers.** Inserts/deletes tamponnés dans un index mémoire court-
  terme, fusionnés périodiquement dans l'index SSD long-terme à un coût
  proportionnel au **seul delta**. La recherche interroge les deux tiers et
  fusionne les résultats. Soutient des milliers d'inserts/deletes/recherches
  concurrents par seconde en conservant **>95 % 5-recall@5**. Papier :
  <https://arxiv.org/abs/2105.09613>. FAIT SOURCÉ — c'est exactement le pattern
  « base immuable + delta mutable fusionné à la recherche » du cahier des
  charges.

- **Dégradation de rappel sur longues séries d'inserts/deletes : réelle et
  documentée.** Vamana montre une dégradation du rappel après cycles répétés de
  suppression/réinsertion, à taille de liste candidate fixe ; d'où la nécessité
  d'une **compaction/refonte périodique** du tier long-terme. Sources :
  <https://arxiv.org/abs/2105.09613>,
  <https://sheng.whu.edu.cn/papers/25bigdata.pdf> (index équilibré actualisable
  pour streaming, 2025). FAIT SOURCÉ.

- **SPFresh (2023) : mise à jour incrémentale « in-place » à l'échelle du
  milliard**, alternative au rebuild global par rééquilibrage local de
  partitions (LIRE). Source :
  <https://www.researchgate.net/publication/374920073_SPFresh_Incremental_In-Place_Update_for_Billion-Scale_Vector_Search>.
  FAIT SOURCÉ.

- **Travaux récents 2025-2026** confirmant que l'ANN incrémental reste un champ
  actif : découplage données/index pour l'efficacité spatiale
  (<https://arxiv.org/pdf/2604.09173>), index équilibré actualisable pour
  streaming stable (<https://sheng.whu.edu.cn/papers/25bigdata.pdf>),
  co-processing CPU-GPU temps réel (<https://arxiv.org/pdf/2601.08528>). FAIT
  SOURCÉ (existence des travaux) ; leur transposabilité pur-Go n'est pas établie
  — INFÉRÉ.

### Applicabilité & verdict

À ~11 k inserts/jour, le régime est **très modéré** au regard des cibles
FreshDiskANN (milliers d'opérations/seconde). Le pattern deux-tiers — graphe
Vamana immuable en arène + petit delta mutable (mémoire ou SQLite) fusionné au
moment de la recherche — est **directement applicable** et sans exigence CGO :
la fusion des résultats de deux recherches est un merge de listes de candidats
en Go pur. La dégradation de rappel impose une **compaction périodique** (refonte
du tier immuable, p. ex. nocturne) plutôt qu'un rebuild continu. **Verdict :
adopter le deux-tiers FreshVamana (immuable + delta), fusion à la recherche, et
planifier une refonte périodique du tier immuable ; le volume 11 k/jour rend un
rebuild complet nocturne parfaitement soutenable si la compaction incrémentale
s'avère complexe à implémenter (arbitrage coût d'ingénierie vs coût de rebuild,
à trancher au sol).**

---

## Synthèse des verdicts

| Nœud | Verdict | Statut |
|---|---|---|
| 1 — Concurrence SQLite | Pool read-only WAL + `mmap_size` + `query_only` = réponse consensus ; branches expérimentales inutiles (et visent l'écriture). Arène mmap gardée pour le chemin chaud figé. | Pattern FAIT SOURCÉ ; supériorité arène-vs-SQLite au sol INFÉRÉE (rejouer le banc après tuning pool). |
| 2 — Quantification/paliers | 1-bit-seul ~76 % insuffisant ; re-classement fp16 obligatoire → arène fp16 validée. Extended RaBitQ 4-5 bit comme palier alternatif. `sqlite-vec` écarté (pas d'ANN + CGO). | FAIT SOURCÉ (Milvus primaire). |
| 3 — ANN incrémental | Deux-tiers FreshVamana (immuable + delta fusionné) applicable pur-Go ; compaction périodique contre la dégradation ; 11 k/j rend le rebuild nocturne soutenable. | Pattern FAIT SOURCÉ (arXiv 2105.09613). |

## Sources clés

- WAL officiel : <https://sqlite.org/wal.html>
- HC-tree / BEGIN CONCURRENT officiel :
  <https://sqlite.org/hctree/doc/begin-concurrent/doc/begin_concurrent.md> ·
  <https://sqlite.org/hctree/doc/hctree/doc/hctree/index.html>
- Maturité branches (mai 2026) :
  <https://powersync.com/blog/sqlite-persistence-on-the-web>
- Bench modernc vs mattn : <https://github.com/cvilsmeier/go-sqlite-bench> ·
  <https://github.com/multiprocessio/sqlite-cgo-no-cgo> ·
  <https://datastation.multiprocess.io/blog/2022-05-12-sqlite-in-go-with-and-without-cgo.html>
- Lecture haute perf Go : <https://dev.to/lovestaco/high-performance-sqlite-reads-in-a-go-server-4on3>
- RaBitQ (papier) :
  <https://www.researchgate.net/publication/381022541_RaBitQ_Quantizing_High-Dimensional_Vectors_with_a_Theoretical_Error_Bound_for_Approximate_Nearest_Neighbor_Search>
- RaBitQ recall Milvus (primaire) :
  <https://milvus.io/blog/bring-vector-compression-to-the-extreme-how-milvus-serves-3%C3%97-more-queries-with-rabitq.md>
- Extended RaBitQ :
  <https://dev.to/gaoj0017/extended-rabitq-an-optimized-scalar-quantization-method-83m>
- sqlite-vec état : <https://marcobambini.substack.com/p/the-state-of-vector-search-in-sqlite>
- FreshDiskANN (papier) : <https://arxiv.org/abs/2105.09613>
- SPFresh : <https://www.researchgate.net/publication/374920073_SPFresh_Incremental_In-Place_Update_for_Billion-Scale_Vector_Search>
- Index streaming actualisable (2025) : <https://sheng.whu.edu.cn/papers/25bigdata.pdf>
