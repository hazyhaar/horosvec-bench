# P1 — semantic_harness v0 : pré-passe hybride BM25 + dense (verdict ÉCHEC documenté)

Date : 2026-07-12. Tâche portefeuille : `019f51ff-e575-777d-9074-adbe77086683`
(campagne META horosvec, mère `019f532b-3e37`). Banc : `cmd/proto-semantic-harness`
(socle composer grok, correction d'alignement et re-run par la session architect).
Moteur `/devhoros/horosvec` en lecture seule (import module).

## Données et définitions fermées

Corpus texte+dates : `data/hn_text_1m.db` (913 783 items, dataset public
ClickHouse, ids 1..1M). Vecteurs : `prefix1m.arena` (fp16 dim 512).
Six shards calendaires 2007-03..08 (5 817 à 10 385 items après filtre
« porte un vecteur »), FTS5 unicode61 + index horosvec db-blob par shard.
200 requêtes stories (PCG(42,7)), requête = titre (BM25) + vecteur d'arène
(dense), self-match exclu. Vérité terrain : brute-force L2 exacte sur l'union
(45 693 items). Chaîne hybride : top-200 BM25 par shard (tokens du titre en
OR) → union ≈ 1 140 candidats → rerank L2 exact → top-10. Baseline dense
même-scope : recherche horosvec par shard, fusion des 6 top-10.

## Incident de mesure — le premier verdict était un artefact

Le premier run rendait recall hybride **0,0300** — exactement le niveau du
hasard (réduction de scope 0,0249). La vérification au sol a réfuté
l'hypothèse de lecture `rang = id−1` dans l'arène : le pipeline d'embedding a
sauté les items morts, et le sidecar `.ids` fait foi (rang 3068 → ext_id 3176,
dérive jusqu'à ext_id 1 071 139 au rang 999 999). Les vecteurs lus ne
correspondaient pas aux textes. Correction : mapping `ext_id→rang` chargé du
sidecar, items et requêtes sans vecteur filtrés. Leçon de méthode : la
vérification initiale de l'alignement portait sur les dix premiers rangs,
seuls points où le mapping est identique — généralisation depuis une
énumération non exhaustive.

## Résultats corrigés (200 requêtes, sortie collée)

| méthode | recall@10 moyen | p50 (ms) | p95 (ms) |
|---|---|---|---|
| hybride | 0,5445 | 28,870 | 62,916 |
| dense | 1,0000 | 14,126 | 15,789 |

Décomposition hybride : BM25 moyen 27,9 ms | rerank moyen 0,8 ms | réduction
de scope moyenne 2,49 %.

## Verdict — ÉCHEC documenté (seuil intact, C7)

Seuil de la tâche : recall@10 hybride ≥ dense − 0,005 ET p95 ≤ 1,2× dense.
Constaté : 0,5445 < 0,9950 et 62,9 ms > 18,9 ms. **Les deux branches échouent.**

Lecture structurelle : à l'échelle des shards de la doctrine semantic_cpu
(≤ ~10 k items), la baseline dense est déjà exacte (sous le seuil brute-force
du moteur) et bon marché (p95 ≈ 16 ms sur 6 shards). La pré-passe lexicale
n'économise rien (le rerank ne coûte que 0,8 ms) et PERD ~45 % du rappel :
les voisins denses d'une story ne partagent souvent aucun token de son titre.
L'hypothèse « le front-end déterministe réduit le scope pour un dense plus
cher » ne paie qu'à des échelles où le dense exact devient coûteux — pas ici.
La stratégie `semcpu_lexical_bm25` reste une brique valable comme VOIE DE
REPLI (dense indisponible) ou pour des requêtes à termes rares — pas comme
pré-passe systématique devant le classeur dense à cette échelle.

Artefacts : `cmd/proto-semantic-harness/` ; shards et sorties brutes sous
`data/semantic-harness/` (gitignoré) et au scratch de session.
