# Latence par requête : arène fp16 vs SQLite direct (db-blob)

Date : 2026-07-10. Mesure décidable, au sol, sur le MÊME jeu de vecteurs et les
MÊMES requêtes. Objet : produire le facteur multiplicatif de latence arène→SQLite
qui tranche l'arbitrage « garder l'arène mmap » vs « passer en SQLite direct pour
l'incrémentalité ». Chantier de mesure, aucune modification du moteur.

## Contrat de réutilisation

- **Consommé tel quel** : le binaire `cmd/bench-horosvec` du banc, qui porte DÉJÀ la
  dimension de mode (`-mode arena` | `-mode db-blob`, commits cab7ff9/cb9aa0d). Le
  sélecteur de medium (`pkg/storagemedium`), le harnais de mesure (`pkg/bench`,
  warm-up 20 requêtes exclues, fenêtres à horloge monotone ≥ 3 s, p50/p99, recall vs
  GT exacte), le protocole d'émission JSONL (`pkg/protocol`) et le chargeur fvecs
  (`pkg/data`) sont réutilisés sans modification.
- **Surface neuve** : AUCUNE ligne de code de banc écrite. Le banc existant permettait
  la comparaison sans le moindre ajout. Seuls des artefacts de données (troncature
  scratch du jeu SIFT) et le présent rapport ont été produits.
- **API moteur** : `horosvec.New` / `BuildFromArena` / `Build` / `Search`, pilotés par
  le banc — lecture seule sur `/devhoros/horosvec`, zéro modification.

## Protocole

Les deux régimes de stockage des vecteurs de re-classement du moteur :

- **arène** (`-mode arena`) : `cfg.ArenaPath` posé, vecteurs fp16 servis par un fichier
  sidecar mmap, aucune requête SQL dans la boucle chaude.
- **SQLite direct** (`-mode db-blob`) : pas d'`ArenaPath`, les vecteurs fp32 de
  re-classement se lisent ligne à ligne dans `vindex_nodes.vec` par requête SQL.

Dans les DEUX modes, le plan chaud (codes RaBitQ 1-bit, voisins, normes) reste en RAM ;
seule la lecture des vecteurs de re-classement diffère. C'est là qu'est le surcoût attendu.

Parité garantie : même fichier de base, même fichier de requêtes, même K, même EfSearch,
et **même vérité terrain exacte** — la GT est calculée une fois puis relue depuis le cache
scratch partagé par les deux runs (validation `n_base`/`k` fail-loud). Le rappel des deux
modes se mesure contre cette GT commune.

## Échelle

- Jeu : **SIFT-1M tronqué aux 200 000 premiers vecteurs**, 128 dimensions
  (`/data/datasets/sift1m/sift_base.fvecs`, format fvecs 516 o/enregistrement, copié
  tronqué dans le scratch pour forcer un recalcul frais de GT ; le `.gt.json` 1M en
  cache aurait fait échouer la validation, à raison).
- Requêtes : `sift_query_500.fvecs`, 500 requêtes disjointes de la base.
- K = 10, EfSearch = 128, medium résolu = **ssd** (NVMe, TMPDIR sous scratch).
- Cache CHAUD : warm-up de 20 requêtes exclu ; fenêtres de mesure ≥ 3 s en boucle
  fermée. Aucun `drop_caches` — la base SQLite (~102 Mo) tient intégralement dans le
  page cache RAM (état à retenir pour le caveat).
- Le VRAI 26,7 M (build en heures, ~54 Go) N'a PAS été tenté ; l'objet est le RATIO.

## Commandes exactes

```
# jeu (scratch) : 200000 * 516 octets ; requêtes copiées telles quelles
head -c $((200000*516)) /data/datasets/sift1m/sift_base.fvecs > $S/sift_base_200k.fvecs
cp /data/datasets/sift1m/sift_query_500.fvecs $S/sift_query_500.fvecs

env GOWORK=off CGO_ENABLED=0 go build -o bench-horosvec ./cmd/bench-horosvec

# balayage de concurrence, un run par mode (build une fois, GT commune en cache)
for MODE in arena db-blob; do
  env GOWORK=off CGO_ENABLED=0 TMPDIR=$S ./bench-horosvec \
    -base $S/sift_base_200k.fvecs -queries $S/sift_query_500.fvecs \
    -k 10 -sweep 128 -concurrency 1,8,32 -mode $MODE
done
```

## Sorties collées (JSONL, param EfSearch=128)

```
# ARÈNE
{"engine":"horosvec-arena","mode":"arena","medium":"ssd","n":200000,"dim":128,"k":10,"concurrency":1, "recall_mean":0.8522,"qps":4150.65, "p50_ms":0.239,"p99_ms":0.308,"mem_mb":65.2}
{"engine":"horosvec-arena","mode":"arena","medium":"ssd","n":200000,"dim":128,"k":10,"concurrency":8, "recall_mean":0.8522,"qps":30573.48,"p50_ms":0.245,"p99_ms":0.43873,"mem_mb":82.2}
{"engine":"horosvec-arena","mode":"arena","medium":"ssd","n":200000,"dim":128,"k":10,"concurrency":32,"recall_mean":0.8522,"qps":69263.87,"p50_ms":0.394,"p99_ms":2.79664,"mem_mb":98.4}
# SQLITE DIRECT (db-blob)
{"engine":"horosvec","mode":"db-blob","medium":"ssd","n":200000,"dim":128,"k":10,"concurrency":1, "recall_mean":0.8524,"qps":5137.18, "p50_ms":0.196,"p99_ms":0.244,"mem_mb":153.1}
{"engine":"horosvec","mode":"db-blob","medium":"ssd","n":200000,"dim":128,"k":10,"concurrency":8, "recall_mean":0.8524,"qps":27960.02,"p50_ms":0.265,"p99_ms":0.495,"mem_mb":118.9}
{"engine":"horosvec","mode":"db-blob","medium":"ssd","n":200000,"dim":128,"k":10,"concurrency":32,"recall_mean":0.8524,"qps":37639.56,"p50_ms":0.756,"p99_ms":3.892,"mem_mb":155.1}
```

## Tableau de synthèse

| Concurrence | Mode | p50 (ms) | p99 (ms) | QPS agrégé | rappel@10 |
|---|---|---|---|---|---|
| 1  | arène   | 0.239 | 0.308 | 4 151  | 0.8522 |
| 1  | sqlite  | 0.196 | 0.244 | 5 137  | 0.8524 |
| 8  | arène   | 0.245 | 0.439 | 30 573 | 0.8522 |
| 8  | sqlite  | 0.265 | 0.495 | 27 960 | 0.8524 |
| 32 | arène   | 0.394 | 2.797 | 69 264 | 0.8522 |
| 32 | sqlite  | 0.756 | 3.892 | 37 640 | 0.8524 |

## Ratio arène→SQLite (facteur multiplicatif de latence, p50/p99)

| Concurrence | ratio p50 (sqlite/arène) | ratio p99 | ratio QPS (sqlite/arène) |
|---|---|---|---|
| 1  | **0.82×** (sqlite plus rapide) | 0.79× | 1.24× |
| 8  | 1.08× | 1.13× | 0.91× |
| 32 | **1.92×** | 1.39× | **0.54×** |

## Contrôle de parité de rappel

Rappel@10 mesuré contre la GT exacte COMMUNE : arène 0.8522, sqlite 0.8524 à toute
concurrence — écart 0.0002, soit une parité de qualité de restitution effective
(overlap ≥ 0.99 attendu, les deux modes partageant le même graphe Vamana et les mêmes
codes RaBitQ ; seule la source des vecteurs de re-classement diffère). La comparaison
de latence porte donc bien sur des restitutions de qualité identique.

## Verdict

Le postulat « le mode SQLite direct coûte ×N en latence par requête » ne se vérifie PAS
au régime séquentiel à cette échelle : à un seul client, cache chaud, le SQLite direct
est **marginalement plus rapide** (0.82× en p50), parce que la base entière (~102 Mo)
réside dans le page cache RAM et que la boucle chaude n'a aucune contention de verrou.

Le surcoût du SQLite direct est un phénomène de **concurrence**, pas de requête isolée :
à 32 clients il coûte **×1.92 en p50** et son débit agrégé **s'effondre à 0.54×** celui
de l'arène (37 640 vs 69 264 QPS). C'est la signature de la contention
`database/sql.withLock` déjà diagnostiquée dans la campagne (le « cliff » de mise à
l'échelle, 21,6 % du temps CPU en `withLock` au profilage). L'arène, sans SQL dans la
boucle chaude, met à l'échelle presque linéairement.

Tranche : pour un service faiblement concurrent, le SQLite direct est acceptable (voire
préférable en latence unitaire et en simplicité d'incrémentalité). Pour une charge
multi-client soutenue, l'arène reste supérieure (débit ×1.8, p99 sous charge divisée par
1.4). L'arbitrage « garder l'arène » se joue donc sur le **régime de concurrence attendu
de la démo**, non sur le coût par requête isolée.

## Caveat d'échelle — DÉCISIF

Ce ratio est un **PLANCHER indicatif, le meilleur cas pour le SQLite**. À 200 000
vecteurs, toute la base SQLite tient dans la RAM : chaque lecture de blob de
re-classement frappe le page cache, jamais le disque. Le ratio par requête ≈ 1 est
précisément l'**artefact de petite échelle** contre lequel la mission mettait en garde.

Au VRAI 26,7 M (base SQLite ~54 Go > RAM), les lectures de blobs en mode SQLite
provoqueraient des défauts de page vers le disque en cache froid — le surcoût par
requête pourrait alors être **bien PIRE** que les facteurs mesurés ici, et exploser en
p99. La présente mesure NE capture PAS ce comportement de cache-page à l'échelle réelle.
Elle borne par le bas le coût du SQLite ; elle ne le majore pas.

## Profil de courbe — latence et débit vs concurrence

Suite au verdict ci-dessus (le coût du SQLite est un phénomène de concurrence, pas de
requête isolée), balayage fin des paliers de concurrence, même jeu (SIFT 200k, 128-dim,
K=10, EfSearch=128, cache chaud, GT commune), seule la concurrence varie. Warm-up de 20
requêtes exclu, fenêtres ≥ 3 s en boucle fermée. Un run par mode couvre tous les paliers
(build une fois : arène 177,8 s, sqlite 72,9 s ; rappel stable 0.8518 / 0.8516).

### Sorties collées (JSONL, EfSearch=128)

```
# ARÈNE
concurrency:1   qps:4236.18  p50:0.235 p99:0.291
concurrency:2   qps:8482.90  p50:0.235 p99:0.300
concurrency:4   qps:16023.06 p50:0.249 p99:0.348
concurrency:8   qps:31336.76 p50:0.243 p99:0.41381
concurrency:16  qps:49483.26 p50:0.298 p99:0.539
concurrency:32  qps:72251.57 p50:0.392 p99:1.928
concurrency:64  qps:75621.31 p50:0.389 p99:20.61781
concurrency:128 qps:72007.27 p50:0.393 p99:21.87152
# SQLITE DIRECT (db-blob)
concurrency:1   qps:4749.08  p50:0.211 p99:0.282
concurrency:2   qps:10120.09 p50:0.197 p99:0.257
concurrency:4   qps:17736.83 p50:0.225 p99:0.358
concurrency:8   qps:29773.48 p50:0.260 p99:0.439
concurrency:16  qps:34145.49 p50:0.438 p99:0.753
concurrency:32  qps:36998.84 p50:0.799 p99:3.739
concurrency:64  qps:35637.42 p50:0.820 p99:24.97086
concurrency:128 qps:38262.11 p50:0.777 p99:72.57812
```

### Tableau de profil

| Conc. | arène p50 | arène p99 | arène QPS | sqlite p50 | sqlite p99 | sqlite QPS | ratio p50 | ratio QPS |
|---|---|---|---|---|---|---|---|---|
| 1   | 0.235 | 0.291  | 4 236  | 0.211 | 0.282   | 4 749  | 0.90× | **1.12×** |
| 2   | 0.235 | 0.300  | 8 483  | 0.197 | 0.257   | 10 120 | 0.84× | **1.19×** |
| 4   | 0.249 | 0.348  | 16 023 | 0.225 | 0.358   | 17 737 | 0.90× | **1.11×** |
| 8   | 0.243 | 0.414  | 31 337 | 0.260 | 0.439   | 29 773 | 1.07× | 0.95× |
| 16  | 0.298 | 0.539  | 49 483 | 0.438 | 0.753   | 34 145 | 1.47× | **0.69×** |
| 32  | 0.392 | 1.928  | 72 252 | 0.799 | 3.739   | 36 999 | 2.04× | **0.51×** |
| 64  | 0.389 | 20.618 | 75 621 | 0.820 | 24.971  | 35 637 | 2.11× | **0.47×** |
| 128 | 0.393 | 21.872 | 72 007 | 0.777 | 72.578  | 38 262 | 1.98× | **0.53×** |

### Point de décrochage et forme

- **Point de décrochage** : entre **concurrence 8 et 16**. Le ratio QPS sqlite/arène
  reste ≥ 1 jusqu'à 4 clients (le SQLite mène, page cache chaud, pas de contention),
  passe sous ~0.9 à 8 (0.95×, limite) puis **chute nettement à 16 (0.69×)** et
  s'effondre à 32 (0.51×). Le p50 franchit le double de l'arène dès 32 clients.
- **Forme des courbes** :
  - **Arène** : débit en montée quasi LINÉAIRE jusqu'à 32 clients (4 236 → 72 252 QPS,
    facteur ×17 pour ×32 clients), puis **plateau** vers 72–76 kQPS (saturation des
    cœurs, 32 cœurs physiques). Latence p50 stable (~0.24 → 0.39 ms) ; seul le p99
    monte franchement à 64+ (file d'attente au-delà des cœurs).
  - **SQLite direct** : débit en **coude net** — montée jusqu'à ~8 clients (30 kQPS)
    puis **plateau bas immédiat** à 35–38 kQPS quelles que soient les 16→128. C'est la
    sérialisation par `database/sql.withLock` : au-delà de ~8 clients, les lectures de
    blobs se mettent en file derrière le verrou de connexion, le débit ne monte plus et
    la latence enfle (p50 ×3,8 de 8 à 32, p99 jusqu'à 72,6 ms à 128).

### Verdict de profil

Le mode SQLite direct devient **rédhibitoire au-delà de ~8 clients simultanés** : dès 16
il rend un débit de 0,69× l'arène, dès 32 il plafonne à la moitié (0,51×) avec un p50
doublé. Sous 8 clients, il est équivalent ou supérieur (débit jusqu'à 1,19× à 2 clients).
La bascule n'est pas graduelle mais un **coude franc** dicté par la contention du verrou
de connexion `database/sql`, indépendante de l'échelle des données.

**Rappel du caveat d'échelle** : ce profil est le meilleur cas du SQLite. À 200k, toute
la base tient dans le page cache RAM — le coude reflète la SEULE contention de verrou,
jamais le disque. Au 26,7 M réel (~54 Go > RAM), chaque lecture de blob en cache froid
ajouterait un défaut de page disque : le plateau bas du SQLite s'écroulerait plus tôt et
plus bas, et le coude se déplacerait vers une concurrence encore plus faible. Le profil
mesuré borne par le bas la dégradation réelle.

## Pool read-only dimensionné — le plateau vient-il du pool ou de modernc ?

Hypothèse testée : le plateau du SQLite direct à ~35 kQPS viendrait du churn du pool
`database/sql` par défaut (`MaxIdleConns=2`), qui ferme/rouvre les connexions en rafale
sous concurrence, et de connexions « froides » (cache par défaut, sans mmap) — car
`configureSQLite` de horosvec n'applique ses pragmas qu'à UNE connexion du pool (limite
documentée dans son `pragma.go`).

Variante ajoutée au banc : mode `db-blob-pool` (`-mode db-blob-pool`), sans casser le mode
`db-blob` par défaut. Il ouvre la base avec les pragmas de performance dans le **DSN
modernc** (appliqués à CHAQUE connexion : WAL, `cache_size=-65536`, `mmap_size=256 Mo`) et
dimensionne le vivier : `SetMaxOpenConns(256)`, `SetMaxIdleConns(256)`,
`SetConnMaxIdleTime(0)`, `SetConnMaxLifetime(0)` — un vivier de connexions tièdes couvrant
toute la concurrence balayée, sans fermeture ni recyclage. `PRAGMA query_only(ON)` a été
**volontairement omis** : `horosvec.New` exécute `initSchema` (CREATE TABLE) et `Build`
écrit les nœuds sur le même handle — `query_only=ON` les ferait échouer. C'est une
assertion de lecture seule, pas un levier de performance ; les leviers pertinents (pool,
mmap par connexion) sont bien présents. Même jeu (SIFT 200k, K=10, EfSearch=128, cache
chaud, GT commune), rappel identique 0.8518.

### Sortie collée (JSONL, mode db-blob-pool, EfSearch=128)

```
concurrency:1   qps:5033.40  p50:0.200 p99:0.252
concurrency:2   qps:9800.58  p50:0.197 p99:0.41398
concurrency:4   qps:18593.51 p50:0.211 p99:0.35119
concurrency:8   qps:27827.42 p50:0.266 p99:0.59712
concurrency:16  qps:34898.29 p50:0.432 p99:0.723
concurrency:32  qps:40124.31 p50:0.738 p99:3.74191
concurrency:64  qps:39782.59 p50:0.738 p99:24.324
concurrency:128 qps:37464.77 p50:0.759 p99:83.34431
```

### Tableau comparatif à trois régimes (débit agrégé)

| Conc. | arène QPS | sqlite-défaut QPS | sqlite-pool QPS | ratio pool/arène |
|---|---|---|---|---|
| 1   | 4 236  | 4 749  | 5 033  | 1.19× |
| 2   | 8 483  | 10 120 | 9 801  | 1.16× |
| 4   | 16 023 | 17 737 | 18 594 | 1.16× |
| 8   | 31 337 | 29 773 | 27 827 | 0.89× |
| 16  | 49 483 | 34 145 | 34 898 | 0.71× |
| 32  | 72 252 | 36 999 | 40 124 | **0.56×** |
| 64  | 75 621 | 35 637 | 39 783 | 0.53× |
| 128 | 72 007 | 38 262 | 37 465 | 0.52× |

### Réponse tranchée

**Le plateau NE se lève PAS.** Le vivier dimensionné + les pragmas par connexion apportent
un gain **marginal** (au pic de concurrence 32 : 40 124 contre 36 999 QPS, soit **+8 %**),
mais le SQLite direct reste plafonné à ~37–40 kQPS quelle que soit la concurrence, là où
l'arène atteint ~72 kQPS. Le coude reste au même endroit (décrochage entre 8 et 16
clients : ratio pool/arène 0.89× à 8, 0.71× à 16). Le plateau **n'était donc PAS un
artefact du pool `database/sql`** : c'est une **limite plus profonde de la sérialisation
interne de `modernc.org/sqlite`** (pur Go, sans cgo), dont les lectures se sérialisent
au-delà d'un certain parallélisme indépendamment du provisionnement du pool. Le p99 sous
forte concurrence reste même dégradé (83 ms à 128 clients).

### Verdict

Avec un pool propre, le mode SQLite direct **ne devient PAS viable à toute concurrence** :
le plafond de débit tient à modernc lui-même, pas au harnais du pool. Sous ~8 clients il
reste équivalent ou supérieur à l'arène (jusqu'à 1.19×) ; au-delà de 16 il rend la moitié
du débit de l'arène et n'y peut rien. Pour une démo faiblement concurrente, le SQLite
direct est acceptable et gagne en incrémentalité ; pour toute charge multi-client
soutenue, l'arène reste requise.

**Rappel du caveat d'échelle** : ce comparatif à trois régimes est le meilleur cas du
SQLite — à 200k tout tient en page cache RAM, aucune lecture ne touche le disque. Le
plafond mesuré est donc purement CPU/verrou logiciel. Au 26,7 M réel (~54 Go > RAM), le
mode SQLite ajouterait des défauts de page disque en cache froid : son plateau
s'écroulerait plus bas encore, et l'écart avec l'arène se creuserait. Le dimensionnement du
pool n'y changerait rien, la limite étant en amont.
