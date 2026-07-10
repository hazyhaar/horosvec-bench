# Refonte UI démo horosvec — interface deux panneaux + contenu HN réel

Date : 2026-07-10. Zone : `cmd/hnbook-serve/index.html` (refonte complète), ledger.
Aucune modification de `server.go` (voir §CSP), du moteur horosvec, du pipeline
d'embedding ni du contrat `/api/search`.

## Golden strate 0 (avant édition)

```
$ cd /devhoros/horosvec-bench && env GOWORK=off CGO_ENABLED=0 go build ./cmd/hnbook-serve/ \
  && env GOWORK=off CGO_ENABLED=0 go vet ./cmd/hnbook-serve/ \
  && env GOWORK=off CGO_ENABLED=0 go test ./cmd/hnbook-serve/ \
  && gofmt -l cmd/hnbook-serve/
ok  	github.com/hazyhaar/horosvec-bench/cmd/hnbook-serve	1.151s
EXIT=0
```

## Sonde CSP (server.go)

```
$ grep -n "Content-Security-Policy|connect-src|Header().Set" cmd/hnbook-serve/server.go
73: w.Header().Set("Content-Type", "text/html; charset=utf-8")
74: w.Header().Set("X-Content-Type-Options", "nosniff")
148: w.Header().Set("Content-Type", "application/json; charset=utf-8")
149: w.Header().Set("X-Content-Type-Options", "nosniff")
```

Aucune directive `Content-Security-Policy` n'est posée. Le fetch navigateur vers
`hacker-news.firebaseio.com` n'est donc pas restreint par une CSP serveur.
Conformément à la consigne (« Si aucune CSP n'est posée, ne rien ajouter »),
`server.go` reste inchangé.

## Contrat /api/search (vérifié, inchangé)

`searchResult{ID string json:"id"; Score float64 json:"score"; TitleSnippet string json:"title_snippet,omitempty"}`.
La refonte ne consomme que `id` et `score` ; `title_snippet` (vide en pratique)
n'est plus utilisé côté front puisque le titre réel provient de l'API HN.

## Gates de sortie (après édition)

```
$ cd /devhoros/horosvec-bench && env GOWORK=off CGO_ENABLED=0 go build -o <bin> ./cmd/hnbook-serve/ \
  && env GOWORK=off CGO_ENABLED=0 go vet ./cmd/hnbook-serve/ \
  && env GOWORK=off CGO_ENABLED=0 go test ./cmd/hnbook-serve/ \
  && gofmt -l cmd/hnbook-serve/
ok  	github.com/hazyhaar/horosvec-bench/cmd/hnbook-serve	(cached)
GOFMT: (aucune sortie — tout formaté)
```

## Preuve d'embed (asset dans le binaire)

`strings` casse les chaînes contenant de l'UTF-8 multi-octet ; la preuve utilise
des marqueurs ASCII uniques de la nouvelle page :

```
strings <bin> | grep -c "detail-empty"              -> 4
strings <bin> | grep -c "hacker-news.firebaseio.com" -> 1
strings <bin> | grep -c "decodeEntities"             -> 4
```

La nouvelle page est bien embarquée via `//go:embed index.html`.

## Choix de conception

- **Layout** : conteneur `.split` en flex (gauche 44 % liste, droite 56 % détail
  en position sticky). Sous 720 px, `flex-direction: column` empile les panneaux ;
  la barre de recherche reste en haut, pleine largeur, hors du split.
- **Source de contenu** : API publique HackerNews Firebase
  (`/v0/item/<id>.json`), appelée EN JAVASCRIPT côté navigateur, jamais côté Go.
  Aucun stockage serveur. Cache mémoire `Map(id -> item|null)` : les fetch de la
  liste peuplent le cache, réutilisé au clic. Item `null`/supprimé géré (titre
  repli « item <id> », aperçu vide, sous-titre « introuvable ou supprimé »).
- **Aperçu gauche** : ~140 premiers caractères du texte décodé ; à défaut de
  texte, l'hôte de l'`url` cible. Placeholder « item <id> » seulement pendant le
  chargement.
- **Panneau droit** : titre, auteur (`by`), date lisible (`time` epoch →
  `toLocaleString`), score HN (`score`), texte intégral décodé, lien cible et
  lien discussion HN.
- **Anti-XSS (dur)** : titres et aperçus rendus par `textContent` uniquement. Le
  `.text` HN (qui contient du HTML) est décodé en TEXTE BRUT via `decodeEntities`
  (textarea `innerHTML=raw` puis lecture de `.value`, `<p>` remplacé par saut de
  ligne), réinjecté via `textContent`. Aucun `innerHTML` de donnée distante, aucun
  `eval`. Les liens portent `rel="noopener noreferrer"` (+`nofollow` pour l'url
  cible externe).

## Négcritères

- N0 : 1 fichier de zone modifié (`index.html`) + ce ledger. OK.
- N1 : 0 dépendance Go nouvelle. OK.
- N2 : moteur horosvec, contrat `/api/search`, pipeline embedding intacts. OK.
- N3 : 0 `innerHTML` de donnée distante non nettoyée. OK.
- N4 : 0 secret. N5 : 0 push, 0 redéploiement. N6 : recherche inchangée. OK.

## Auto-audit adversarial (subagent auditeur, mode secu-deep)

Verdict : VERT sur tous les sinks HTML (chaque `innerHTML` porte une constante
littérale ; toute donnée HN passe par `textContent` ; `decodeEntities` via
textarea RCDATA jugé sûr ; item null géré ; contrat `/api/search` intact ;
responsive présent). Un finding SOFT : `a.href = item.url` posé sans valider le
schéma (vecteur `javascript:` conditionné à un clic, atténué par rel/target).

Résolution (commit 51175c8) : le lien cible n'est posé que si le protocole est
`http:`/`https:` (parse `new URL`, sinon href non posé) — bloque `javascript:`
et `data:`. Rebuild + vet + test + gofmt verts, embed confirmé (detail-empty=4).
