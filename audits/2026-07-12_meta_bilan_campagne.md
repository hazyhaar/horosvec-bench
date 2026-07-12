# Bilan de campagne META horosvec (mère 019f532b-3e37) — clôture C11

Date : 2026-07-12. Campagne forgée par l'arbre `treemaking:forge_de_tache`
(tri-adversarialisation opus/hy3/grok-composer à la forge, après
adversarialisation tri-modèle des cinq filles en session le 2026-07-11).
Porteur unique : architect-session-3d1d35ad. Périmètre respecté (C1) :
tous les commits sous /devhoros/horosvec et /devhoros/horosvec-bench.

## Verdicts des cinq filles

| Fille | Verdict | Commit(s) | Fait central |
|---|---|---|---|
| P0 validation Vamana | **CLOSE — OUI** | `cb18583` | graphe bâti quantifié = fp32 au bruit près ; inserts +50 % → Δrecall max 0,0050 ; RAM shard-mois bornée ; db-blob qualifiable d'incrémental |
| P1 semantic_harness v0 | **CLOSE — ÉCHEC documenté** | `c5d37a7` | au régime shard, le dense est exact et bon marché ; la pré-passe BM25 ajoute 28 ms et perd 45 % de rappel |
| P2 SIMD marche | **CLOSE — ABANDON documenté** | `f995981` | noyau AVX2 ×1,26 isolé ; +5,3 %/+0,4 % e2e conc 32 < seuil 15 % — plafond = bande passante mémoire |
| P3 doc/portabilité | **CLOSE** | `f5c8ff9` | doc.go conforme à la rotation réelle ; mmap derrière tags, Windows compile-only déclaré |
| P4 frontière produit | **CLOSE** | `f5c8ff9`, `9e6b56c` | deux artefacts (travail db-blob / publication arène par shard) ; conditionnel « incrémental » levé par P0 |

Portes tenues : C2 (P4 close avant P2/P3), C4/C5 (recalls et levée du
conditionnel après verdict P0 daté), C6 (P3/P4 même commit doc), C7 (deux
échecs clos seuils intacts, leçons gravées), C9 (baseline tasktree55 verte à
chaque clôture, revérifiée à la présente), C10 (dette rouge horos55 isolée,
compte stable à six).

## Leçons gravées (lecon_ledger)

1. Forge : le graveur `fg` ne transporte pas `refs` (mère née sans ancrage) ;
   bras `horos55-test` ignore `{chemin}` ; échec de feuille sans
   `failure_detail` ; classes d'échec instables.
2. Arbre d'implémentation : feuille agentique sans consigne = freewheel
   (adoption du chantier d'autrui, rapport de succès trompeur) — anti-freewheel
   dur demandé en diff de variante.
3. semantic_cpu : `semcpu_lexical_bm25` repositionnée en voie de repli au
   régime shard ; le régime d'échelle devient variable d'entrée du harnais.
4. Perf chemin chaud : un seuil de gain DOIT nommer concurrence ET échelle ;
   piste réelle restante = vectoriser le rerank au régime shard (forge neuve).
5. Méthode : rang d'arène ≠ id−1, le sidecar `.ids` fait foi ; un recall égal
   à la réduction de scope est une signature de désalignement ; l'import TSV
   sqlite silencieusement lacunaire (55 % de pertes) — préférer JSON.

## Diffs de variante tri-modèle (matière tree_amend)

Issus de l'adversarialisation tri-modèle des critères (grok-composer,
grok-build, hy3) : le P0-prérequis exigé unanimement s'est avéré la meilleure
décision de la campagne (il a validé l'incrémental AVANT que P4 ne le grave) ;
l'objection « P2 double-banc incohérent avec P4 » a été intégrée et a
correctement prédit l'issue (l'arène n'a pas porté le verdict) ; l'objection
hy3 « 2,5 % d'écart dans le bruit » a reformulé P4 en « parité » — tenu.

## Dettes restantes au portefeuille (hors campagne)

Six tests rouges horos55 (dette dédiée) ; dérive de schéma `tache_claim`
(`prise_par` non déclaré) ; orchestrateur inter-tâches inexistant ;
verbe de re-parentage absent (`refs`/`parent_id`).
