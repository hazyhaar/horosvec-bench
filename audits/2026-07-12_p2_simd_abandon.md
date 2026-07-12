# P2 — noyau SIMD de la marche : rapport d'abandon (gabarit imposé)

Date : 2026-07-12. Tâche portefeuille : `019f5200-471d-7590-a8cc-3b54370e411f`
(campagne META horosvec, mère `019f532b-3e37`). Décision : **ABANDON** (gain
mesuré sous le seuil), critère (e) de la tâche.

## Profil (préalable obligatoire du critère)

- Conversion fp16→fp32 : absente du top-12 du profil CPU (< 5 %) → retirée du
  scope dès le préalable, conformément au critère.
- Échelle 10 k (benchmark moteur) : `l2DistanceSquared` (rerank) domine à
  48,6 % flat ; la LUT de marche ne pèse que 7,7 %.
- Échelle 1 M (audit hotpath 2026-07-10) : la marche domine (~70 %), bornée
  par la bande passante mémoire du plan chaud.

## Hypothèse testée

Un noyau assembleur AVX2 (`VGATHERQPD`, deux chaînes de gather indépendantes)
de la distance LUT de la marche rapporte ≥ +15 % de QPS bout-en-bout à
concurrence 32 sur db-blob.

## Gains mesurés

- **Noyau isolé** (dim 512, épinglé P-core, 4 passes × 2 M itérations) :
  scalaire 14,7-15,3 ns/op → AVX2 11,6-12,3 ns/op = **×1,26**. Le déroulé par
  8 avec double chaîne de gather n'apporte rien de plus (limité en débit du
  gather, pas en latence).
- **Bout-en-bout** (proto-hotpath, 200 k × dim 512, moteur intégré derrière
  tag `GOAMD64=v3`, parité vérifiée, suites complètes vertes v1 et v3) :

| banc | conc | v1 (QPS) | v3 AVX2 (QPS) | gain |
|---|---|---|---|---|
| arena | 1 | 1 673 | 1 874 | +12,0 % |
| arena | 32 | 25 194 | 25 610 | +1,7 % |
| dbblob-allcache | 1 | 2 011 | 2 366 | **+17,7 %** |
| dbblob-allcache | 32 | 30 029 | 31 620 | **+5,3 %** |
| dbblob-halfcache | 1 | 2 013 | 2 396 | +19,0 % |
| dbblob-halfcache | 32 | 30 536 | 30 665 | **+0,4 %** |

## Décision binaire

Seuil : ≥ +15 % QPS à conc 32 sur db-blob. Mesuré : +5,3 % / +0,4 %.
**ABANDON.** Le gain mono-fil (+17,7 %) est réel mais s'évapore sous
concurrence : le plafond à conc 32 est la bande passante mémoire partagée du
plan chaud, pas le débit de calcul du noyau — un noyau plus rapide attend la
même mémoire. C'est la confirmation chiffrée du verdict du banc SIMD du
2026-07-10 (« le SIMD frappe le mauvais goulot »), étendue cette fois à
l'assembleur avec gather matériel, que le banc pur-Go ne pouvait pas exprimer.

## Sort des artefacts

L'intégration moteur (tags `amd64.v3`, repli pur-Go) a été construite,
validée (parité 1e-9, suites vertes deux modes) puis **retirée** du moteur —
un noyau qui échoue sa propre justification ne s'expédie pas. Le prototype,
ses tests de parité et ses mesures survivent au banc :
`kernels/simdbench/kernels_avx2.{s,go}` + ce rapport. Piste restante
documentée (hors scope P2, échelle shard) : vectoriser `l2DistanceSquared`
(rerank, 48,6 % du CPU au régime shard, ×2,4-2,9 déjà mesuré en pur-Go SIMD) —
relèverait d'une nouvelle forge avec seuil posé au régime shard.
