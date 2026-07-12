# P0 — Validation du graphe Vamana en régime réel (quantifié + inserts dynamiques)

Date : 2026-07-12. Tâche portefeuille : `019f51ff-5fb1-7d04-a029-8d06ecb5e0f6`
(campagne META horosvec, mère `019f532b-3e37`). Exécution : banc
`cmd/proto-vamana-validation` (composer grok headless pour le socle, extension
`HVP0_ONLY=c-mois` et vérification des gates par la session architect).
Moteur `/devhoros/horosvec` en lecture seule stricte (HEAD `f5c8ff9`).
Corpus réel : `/inference/hnbook/bench_final/prefix1m.arena` (HVARENA1,
dim 512, embeddings HackerNews).

## (a) Graphe bâti en distances quantifiées vs fp32 — shard-jour 11 600

Construction Vamana portée côté banc (portage fidèle rotation Hadamard graine 42
+ RaBitQ 1-bit depuis `rotation.go`/`rabitq.go`) ; recherche identique pour les
deux graphes (marche 1-bit + rerank exact top-128, même ef) ; vérité terrain
brute-force exacte fp32, 200 requêtes.

| graphe | recall@10 | écart vs fp32 |
|---|---|---|
| construction fp32 | 0,9995 | 0,0000 |
| construction RaBitQ 1-bit | 1,0000 | −0,0005 |

**La construction quantifiée ne dégrade pas le rappel à cette échelle** (écart
dans le bruit). L'inconnue nommée par le gel multi-bit (« le 0,99 mesuré sur un
graphe bâti fp32 ») est levée pour le shard-jour ; l'extrapolation au-delà
reste à mesurer si un besoin l'exige.

## (b) Inserts dynamiques db-blob — recall et débit vs rebuild

Build à 50 % du shard-jour (5 800), inserts par paliers via l'API publique
(`Insert`), GT recalculée à chaque palier, comparaison au rebuild complet.

| palier | n | recall incr. | recall rebuild | QPS incr. | QPS rebuild | Δrecall |
|---|---|---|---|---|---|---|
| 0 % | 5 800 | 0,9995 | 0,9995 | 4 919 | 4 888 | 0,0000 |
| 10 % | 6 380 | 0,9980 | 0,9990 | 4 686 | 4 493 | 0,0010 |
| 30 % | 7 540 | 0,9960 | 1,0000 | 4 541 | 4 615 | 0,0040 |
| 50 % | 8 700 | 0,9930 | 0,9980 | 4 643 | 4 466 | 0,0050 |

Chute maximale rebuild−incrémental : **0,0050 < seuil d'alerte 0,01**. Débit de
recherche équivalent.

## (c) Pic RSS shard-mois — build 175 000 + inserts 87 500 (mesuré, run 1 h 27)

```
build 50% shard-mois (n=175000): HeapAlloc=1380.0 MiB  Sys=2556.2 MiB  (build en 24m58s)
après inserts +50% (n=262500):   HeapAlloc=965.9 MiB   Sys=4498.6 MiB  (inserts en 1h01m43s)
```

La RAM tient très largement sur la machine 62 GiB. Fait notable pour
l'exploitation : le débit d'INSERTION (~23 vec/s, ~42 ms/vec) est ~5× plus
lent que le build (~8,6 ms/vec) — un delta quotidien HN (11 600) s'insère en
~8 minutes, tenable en cadence nocturne ; une reprise mensuelle préférera le
rebuild.

## (d) Verdict binaire

**db-blob qualifiable d'incrémental : OUI** — chute max de recall 0,0050
(seuil 0,01), RAM bornée, débit de recherche préservé. Le conditionnel posé
par `horosvec/docs/ARCHITECTURE.md §4` (frontière produit P4) peut être levé.

Réserve de portée : (a)/(b) mesurés au shard-jour ; le rappel sous inserts au
shard-mois n'a pas été mesuré (coût GT), seule sa RAM l'a été. Artefacts :
`cmd/proto-vamana-validation/` ; sorties brutes au scratch de session
(rapport incrémental + `c_mois.out`).
