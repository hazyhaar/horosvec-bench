# Fin du 502 au démarrage : liaison immédiate du port + préchauffage auto-rafraîchi

Date : 2026-07-10
Périmètre : `cmd/hnbook-serve/` (démarrage, garde de disponibilité, page de préchauffage)

## Cause racine (vérifiée au sol)

Le serveur ouvrait l'index horosvec (graphe Vamana ~26,7 M nœuds, ~90 s) AVANT
`ListenAndServe`. Pendant ce chargement aucun port n'était lié, si bien que nginx
(`proxy_pass 127.0.0.1:8472`) renvoyait un 502 à tout visiteur pendant ~90 s à chaque
redémarrage.

## Correctif

- `main.go` : `net.Listen("tcp", -addr)` est appelé IMMÉDIATEMENT, avant tout chargement
  d'index. L'index est ensuite ouvert dans une goroutine ; `httpSrv.Serve(ln)` sert le
  listener déjà lié dès le départ. Le chargement des titres (rapide, lecture de fichier)
  reste synchrone.
- Publication atomique de l'index : `server.idxHolder atomic.Pointer[searcher]`. Les
  gestionnaires lisent via `index() (searcher, bool)` — l'index n'est jamais déréférencé
  avant d'être prêt (aucune course, aucun nil-deref pendant le chargement).
- Échec de chargement fail-loud : `server.loadErr atomic.Pointer[string]` renseigné, log
  d'erreur, puis `os.Exit(1)` après un délai de grâce (`loadErrorGrace = 30 s`) — jamais
  de préchauffage éternel silencieux (N6).
- Fermeture de l'index à l'arrêt publiée atomiquement (`server.onClose`) pour éviter la
  course sur la fonction de fermeture entre la goroutine de chargement et le point
  d'assemblage.

## Comportement des routes pendant le préchauffage (index non publié)

- `GET /` → 200 + page `warming.html` embarquée (`//go:embed`), charte sombre identique,
  auto-rafraîchie par `<meta http-equiv="refresh" content="5">`. En cas d'échec de
  chargement : 503 + page d'indisponibilité brève (pas de fausse promesse).
- `GET /api/search` et `GET /api/preview` → 503 + `{"status":"warming"}` + `Retry-After: 5`.
- `GET /healthz` → 503 pendant le préchauffage, 200 une fois prêt.

Une fois prêt : comportement nominal strictement inchangé (search, preview, page deux
panneaux).

## Choix /healthz = readiness (motivé)

`/healthz` est promu de vivacité pure à sonde de DISPONIBILITÉ : c'est le signal honnête
que lisent simultanément le rafraîchissement de la page de préchauffage, la supervision et
le proxy amont. Une seule sonde cohérente évite d'introduire un `/readyz` distinct et le
risque d'un `/healthz` qui mentirait « ok » alors que la recherche renvoie 503. Choix
retenu plutôt qu'un ajout `/readyz` (plus simple, un seul contrat de disponibilité).

Note nginx : si `proxy_intercept_errors on` était actif, nginx pourrait substituer sa
propre page d'erreur au 503 des routes d'API ; le 200 de `GET /` (page de préchauffage)
n'est jamais intercepté et parvient tel quel au visiteur. Le comportement voulu suppose
`proxy_intercept_errors off` (défaut) pour laisser passer le 503 JSON aux appels d'API.

## Golden strate 0 (avant édition)

```
$ env GOWORK=off CGO_ENABLED=0 go build ./cmd/hnbook-serve/  # ok
$ env GOWORK=off CGO_ENABLED=0 go vet ./cmd/hnbook-serve/    # ok
$ env GOWORK=off CGO_ENABLED=0 go test ./cmd/hnbook-serve/   # ok (cached)
$ gofmt -l cmd/hnbook-serve/                                 # (vide)
```

## Gates (après édition)

```
$ env GOWORK=off CGO_ENABLED=0 go build ./cmd/hnbook-serve/  # ok
$ env GOWORK=off CGO_ENABLED=0 go vet ./cmd/hnbook-serve/    # ok
$ env GOWORK=off CGO_ENABLED=0 go test ./cmd/hnbook-serve/   # ok  1.478s
$ gofmt -l cmd/hnbook-serve/                                 # (vide)
```

## Test d'état DÉCIDABLE (assert au sol)

`TestWarmingState` (index non publié → publié) et `TestWarmingLoadError` (état d'erreur) :

```
$ env GOWORK=off CGO_ENABLED=0 go test ./cmd/hnbook-serve/
ok  	github.com/hazyhaar/horosvec-bench/cmd/hnbook-serve	1.478s
```

Assertions : préchauffage → `GET /` = 200 + corps page warming ; `/api/search` = 503 +
`{"status":"warming"}` + `Retry-After: 5` ; `/api/preview` = 503 ; `/healthz` = 503.
Après `setIndex` → `/healthz` = 200, `/api/search` = 200. État d'erreur → routes 503 +
`{"status":"error", ...}`.

## Course (profil cgo de test)

```
$ env GOWORK=off CGO_ENABLED=1 go test -race -run 'TestWarmingState|TestSearchOK|TestWarmingLoadError' ./cmd/hnbook-serve/
ok  	github.com/hazyhaar/horosvec-bench/cmd/hnbook-serve	91.412s
```

Aucune course détectée sur la publication/lecture atomique de l'index et du drapeau.

## Preuve d'embarquement (go:embed)

```
$ strings hnbook-serve | grep -c "préchauffage\|warming"
6
```
