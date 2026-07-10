# Traduction anglaise de la démo publique horosvec + correction du libellé de couverture

Date : 2026-07-10
Zone : `cmd/hnbook-serve/` (index.html, warming.html, server.go, preview.go)

## Objet

Traduire l'intégralité du texte visible de la démo publique horosvec en anglais
(public cible : développeurs Hacker News / r/golang) et corriger le bandeau de
couverture du corpus, trompeur : le corpus réel va de 2006 à octobre 2021 (items
HN d'id 1 à 28738664, 26,7 M items), la date « snapshot 2026-07-08 » n'étant que
la date de build de l'index.

## Golden strate 0 (avant édition)

```
$ env GOWORK=off CGO_ENABLED=0 go build ./cmd/hnbook-serve/ \
  && go vet ./cmd/hnbook-serve/ && go test ./cmd/hnbook-serve/ && gofmt -l cmd/hnbook-serve/
ok  github.com/hazyhaar/horosvec-bench/cmd/hnbook-serve  (cached)
(gofmt : aucune sortie)
GOLDEN_OK
```

## Modifications (texte uniquement, 0 changement de logique)

- `index.html` : `lang="en"`, titre, h1, sous-titre, **bandeau de couverture**
  (« Hacker News corpus · 2006 → October 2021 · 26.7M items · horosvec v0.7.0
  index · CPU-only »), placeholder, bouton Search, libellés de tri
  (Relevance/Newest/Oldest), note honnête de tri, pagination (Page X of Y,
  Previous/Next, N results), panneau détail (état vide, by <author>, Hacker News
  discussion, Target link, Loading preview…, Item not found or deleted), footer,
  et toutes les chaînes produites par le script (Searching…, No results.,
  N results · X ms (embedding Y ms), Too many requests…, Embedding service
  unavailable., Error N.). Format de date unifié en anglais court « 3 Oct 2021 »
  (table de mois) ; `toLocaleString("en-US")` dans le sous-titre détail.
- `warming.html` : `lang="en"`, titre, h1 « Warming up », corps « Loading the
  semantic index (26.7 million items). Search will be available in a few
  moments. », « This page refreshes automatically. ».
- `server.go` : littéraux de message user-visibles uniquement — page HTML de
  repli 503 (Service temporarily unavailable / Loading the index failed),
  healthz (warming / error:), messages d'erreur JSON `{"error":...}`
  (too many requests, missing q parameter, query too long, embedding service
  unavailable, search unavailable, missing url parameter, url too long) et les
  clés de log slog. **Clés JSON, routes, statuts, logique : inchangés.**
- `preview.go` : littéraux `res.Error` surfacés en JSON (invalid url,
  target refused, invalid request, target unreachable, status N, non-HTML
  content, read interrupted). Les erreurs internes de validation SSRF
  (fmt.Errorf) ne sont jamais surfacées à l'usager (collapse en « target
  refused ») et restent inchangées ; anti-SSRF intact.

## Gates (après édition)

```
$ env GOWORK=off CGO_ENABLED=0 go build ./cmd/hnbook-serve/ \
  && go vet ./cmd/hnbook-serve/ && go test ./cmd/hnbook-serve/ && gofmt -l cmd/hnbook-serve/
ok  github.com/hazyhaar/horosvec-bench/cmd/hnbook-serve  2.066s
(gofmt : aucune sortie)
GATES_OK
```

## Preuve

```
$ strings hnbook-serve | grep -ci "semantic search\|October 2021\|Newest"
4                                    # anglais présent
$ strings hnbook-serve | grep -ci "recherche sémantique\|Plus récent\|Sélectionner"
0                                    # aucun résidu français visible
$ strings hnbook-serve | grep -c "snapshot 2026"
0                                    # N6 : le libellé trompeur a disparu
$ strings hnbook-serve | grep -c "October 2021"
1                                    # couverture réelle affichée
```

## Note

Résidus français restants : uniquement dans des **commentaires** de code JS
(index.html) et Go — non rendus, hors « texte visible » (scope mission). Non
traduits par discipline de périmètre ; à signaler si une passe de nettoyage des
commentaires du dépôt public est souhaitée.

## Négcritères

- N0 0 fichier hors zone (+ ce ledger). N1 0 dépendance ajoutée. N2 0 changement
  de logique/route/clé JSON (moteur/preview/embedclient/warming-logique intacts).
  N3 anti-XSS intact (textContent, validation de schéma d'URL/image, aucun
  innerHTML de donnée distante). N4 0 secret. N5 0 push, 0 redéploiement.
  N6 bandeau corrigé (2006 → October 2021, plus de « snapshot 2026 »).
