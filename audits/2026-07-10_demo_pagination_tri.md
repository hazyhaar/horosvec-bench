# Démo horosvec — pagination, tri par date, date par résultat, façade pleine largeur

Date : 2026-07-10
Zone : `cmd/hnbook-serve/index.html`, `cmd/hnbook-serve/server.go`, `cmd/hnbook-serve/serve_test.go`

## Contrat de réutilisation

- Réutilisé : le contrat `/api/search` (forme de réponse `{results:[{id,score}],latency_ms,embed_ms}`
  inchangée), l'enrichissement navigateur existant via l'API publique HN (`item/<id>.json`, champ
  `time` déjà disponible), le cache mémoire `itemCache` (Map id→item) déjà présent, les gardes
  anti-XSS existantes (`textContent`, `safeImageUrl`, `safeExternalUrl`, `decodeEntities`).
- Surface neuve : côté navigateur, un pool de préchargement à concurrence bornée (`prefetchItems`,
  POOL=8), le tri côté navigateur (`sortedResults`), la pagination côté navigateur (`renderPage`),
  le rendu de la date par carte (`dateOf`/`itemTime`). Côté serveur, la résolution du paramètre `k`
  (`parseTopK`, plafond `maxTopK=100`).
- Décision réutiliser-vs-construire : aucune dépendance nouvelle (N1), aucun framework. Tout est
  vanilla JS dans la page embarquée, cohérent avec l'existant.

## Constat serveur (sonde avant édition)

`grep` sur `server.go` : le paramètre `k` n'était PAS lu — la recherche utilisait toujours `s.topK`
(fixé par le flag `-topk`, défaut 10). Impossible de pagier sur 60 résultats sans que le serveur
lise `k`. Ajout minimal : `parseTopK(k, s.topK)` avec plafond 100 (borne anti-abus, pas illimité).
Absent/vide/invalide/≤0 → fallback `s.topK` ; > 100 → écrêté à 100. Forme de réponse inchangée.

## Golden strate 0 (avant édition)

```
env GOWORK=off CGO_ENABLED=0 go build ./cmd/hnbook-serve/  -> ok
env GOWORK=off CGO_ENABLED=0 go vet   ./cmd/hnbook-serve/  -> ok
env GOWORK=off CGO_ENABLED=0 go test  ./cmd/hnbook-serve/  -> ok
gofmt -l cmd/hnbook-serve/                                 -> (vide)
EXIT=0
```

## Gates de sortie

```
env GOWORK=off CGO_ENABLED=0 go build ./cmd/hnbook-serve/  -> ok
env GOWORK=off CGO_ENABLED=0 go vet   ./cmd/hnbook-serve/  -> ok
env GOWORK=off CGO_ENABLED=0 go test  ./cmd/hnbook-serve/  -> ok  (TestParseTopK, TestSearchKCap + suite existante)
gofmt -l cmd/hnbook-serve/                                 -> (vide)
```

Preuve embed :
```
strings hnbook-serve | grep -ci "page\|Plus récent\|pertinence"  -> 630  (> 0)
```

## Vérification visuelle au sol (uishot / chromedp headless)

- Statique large 1600×900 : conteneur pleine largeur (max-width 1500px), barre de recherche pleine
  largeur, deux panneaux étalés (gauche ~42% / droite ~58%), listbar+pager masqués avant recherche.
  `console: []`.
- Statique étroit 500×900 : empilement vertical, panneau détail sous la liste. `console: []`.
- Correctif : l'attribut `hidden` était défait par `display:flex` des `.listbar`/`.pager`
  (specificité CSS) — ajout de `[hidden]{display:none !important}`. Re-screenshot confirme les
  contrôles masqués tant qu'aucune recherche n'a eu lieu.
- Scénario dynamique (harness mock-fetch, 60 résultats synthétiques) : recherche → 10 cartes,
  `Page 1 / 6 · 60 résultats`, date affichée par carte ; « Suivant » → `Page 2 / 6` ; tri
  « Plus récent » → retour page 1, id60 (date la plus récente) en tête ; tri « Plus ancien » →
  id1 en tête. Toutes assertions `ok:true`, `console: []`. Note de tri honnête affichée :
  « Tri des 60 résultats pertinents retrouvés (pas du corpus global). »

## Choix de conception

1. Largeur : `main` max-width 1100→1500px, padding latéral 2rem ; split 42/58 ; breakpoint 720px
   conservé (empilement).
2. Pagination : entièrement côté navigateur sur les K résultats déjà récupérés (le serveur n'est
   pas resollicité). 10 par page, boutons Précédent/Suivant + indicateur « Page X / Y · N résultats ».
3. Préchargement : pool borné à 8 fetch HN en vol (jamais 60 simultanés). Page visible préchargée
   en priorité, reste complété en fond ; re-rendu de la page si un tri par date est actif quand les
   dates arrivent.
4. Tri : sélecteur Pertinence (ordre serveur) / Plus récent / Plus ancien, réordonnancement de
   l'ensemble des K puis re-pagination. Items sans date poussés en fin. Libellé honnête near-select.
5. Anti-XSS : la date passe par `textContent` (`dateOf` → JJ/MM/AAAA construit numériquement,
   aucune donnée distante en innerHTML). Aucune régression sur les gardes existantes.
6. Cap k : `parseTopK` plafonné à 100, testé unitairement (TestParseTopK) et de bout en bout
   (TestSearchKCap : k=60 honoré, k=500 écrêté à 100, pas d'erreur).

## NEGCRITERIA

N0 zone respectée (index.html, server.go pour le seul cap k, serve_test.go, ce ledger). N1 zéro
dépendance nouvelle. N2 moteur/preview/embedclient/warming intacts ; forme `/api/search` inchangée.
N3 zéro innerHTML de donnée distante. N4 zéro secret. N5 aucun redéploiement (binaire bâti, l'architecte
déploie). N6 fetch HN pool borné. N7 tri honnêtement libellé (résultats pertinents).

## Auto-audit adversarial (subagent auditeur, secu-deep + quality-bench)

Six lentilles vertes, un finding SOFT sur la lentille 2 (concurrence) : `fetchItem` ne peuplait
`itemCache` qu'à la résolution, si bien que `buildCard` (10 cartes de la première page) ET
`prefetchItems(firstIds)` déclenchaient chacun un fetch pour les mêmes ids — double-fetch de la
première page, pic réel ~18 en vol au lieu du plafond annoncé.

Correctif appliqué : registre `inFlight` (id → Promise) qui dé-duplique les requêtes concurrentes
du même item ; un id n'est demandé au réseau qu'une fois à la fois, la borne du pool porte alors sur
des requêtes réellement distinctes. Re-vérifié au sol (harness mock instrumenté) : sur 60 résultats,
exactement 60 fetch distincts (0 double-fetch), pic in-flight ≤ 12 (jamais 60), le changement de tri
ne re-fetch rien. `console: []`. Aucun finding hard, aucune faille XSS, cap k non contournable.
