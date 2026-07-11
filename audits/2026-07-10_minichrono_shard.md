# Mini-chronomètre construction d'index horosvec — grains de shard temporel

Machine locale (32 coeurs, 62 Gio RAM totale, ~43 Gio disponible au lancement).
Voie CPU pur Go, `GOWORK=off CGO_ENABLED=0`, Vamana native (`hnbook-build`), dim=512, fp16.
Rappel du run précédent (non refait) : 100k ~12 min (index 71 Mo), 300k ~35 min (215 Mo),
1M tué après >78 min (probable OOM). Le présent run mesure PROPREMENT les tailles de shard
réalistes : 11 600 (jour), 50 000, 100 000, 300 000 (mois). 1M explicitement exclu.

Écriture incrémentale : chaque ligne ci-dessous est ajoutée immédiatement après la fin du
build correspondant, jamais tenue en mémoire jusqu'à la fin du run.

## Résultats mesurés

| N | temps build (wall) | RSS pic | GOMAXPROCS | statut |
|---|---|---|---|---|
| 11 600 (jour) | 1:05.08 (65,08 s) | 56 956 Ko (~55,6 Mio) | 32 (1062 % CPU) | OK |
| 50 000 | 5:40.76 (340,76 s) | 199 044 Ko (~194,4 Mio) | 32 (1082 % CPU) | OK |
| 100 000 | 11:51.73 (711,73 s) | 373 296 Ko (~364,5 Mio) | 32 (1090 % CPU) | OK |
| 300 000 (mois) | 36:32.63 (2192,59 s) | 856 404 Ko (~836,3 Mio) | 32 (1097 % CPU) | OK |

## Fit et extrapolations

Coût unitaire (temps de build par vecteur), croissance douce confirmée :

| N | ms / vecteur | Ko RSS / vecteur |
|---|---|---|
| 11 600 | 5,61 | ~4,9 |
| 50 000 | 6,82 | ~4,0 |
| 100 000 | 7,12 | ~3,7 |
| 300 000 | 7,31 | ~2,9 |

Loi de puissance ajustée `t = a·N^b` sur les quatre points : **b ≈ 1,08** (super-linéaire
léger — le coût par vecteur monte lentement, cohérent avec un degré de graphe borné et une
recherche de voisins en ~log N). Le RSS croît quasi linéairement (~2,9–4,9 Ko/vecteur, la
part fixe s'amortit avec N).

Extrapolations depuis le point 300k (b = 1,08), à interpréter comme des ordres de grandeur,
jamais comme des faits mesurés :

| Grain | N | Temps build extrapolé | RSS extrapolé | Verdict |
|---|---|---|---|---|
| Jour (mesuré) | 11 600 | 1 min (mesuré) | 56 Mio (mesuré) | shard quotidien trivial, autonome |
| Mois | ~350 000 | ~43 min | ~1 Gio | reconstructible en cadence nocturne |
| Année agrégée | ~1 000 000 | ~2 h 15 | ~2,8 Gio | RAM tient ; mur de TEMPS seul |
| Monolithe HN | 26 700 000 | ~3 jours | ~75 Gio | **double mur** : temps (jours) ET RAM (> 62 Gio) |

Conclusion : le grain **jour (~1 min)** rend la mise à jour incrémentale nocturne triviale et
entièrement autonome sur CPU ; le grain **mois (~43 min)** reste tenable en lot de nuit ; le
**monolithe 26,7 M est mort par construction** (mur de temps ET de RAM simultanés). La voie
retenue — sharder par date et concaténer/merger au retrieval plutôt que fusionner des index —
tient précisément parce qu'aucun shard ne franchit ces murs.
