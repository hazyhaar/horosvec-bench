# Aperçu Open Graph durci pour items à URL externe — hnbook-serve

Date : 2026-07-10
Dépôt : /devhoros/horosvec-bench (PUBLIC), module github.com/hazyhaar/horosvec-bench, HORS go.work.
Objet : point d'accès serveur `GET /api/preview?url=<URL>` mandataire d'aperçu Open Graph
(le navigateur ne peut récupérer les métadonnées d'une page externe, politique CORS), branché
au panneau droit. Durcissement anti-SSRF au cœur.

## Contrat de réutilisation

- Consommé (sans dupliquer) : le limiteur de débit par IP `ipLimiter` de `ratelimit.go`
  (route /api/preview soumise au MÊME seau que /api/search), les aides serveur `clientIP`,
  `writeJSON`, `writeError` de `server.go`, le patron de test (index réel, `newIPLimiter`) de
  `serve_test.go`. Décodage d'entités par la stdlib `html.UnescapeString`.
- Surface neuve : `cmd/hnbook-serve/preview.go` (fetch durci + parse OG stdlib + garde SSRF +
  cache), `cmd/hnbook-serve/preview_test.go`, route + handler `handlePreview` + previewer
  paresseux dans `server.go`, bloc front `/api/preview` dans `index.html`.
- Décision réutiliser-vs-construire : aucun parseur HTML tiers (x/net INTERDIT) → scan borné par
  expressions régulières sur les balises `<meta>` et `<title>`, robustesse > exhaustivité.
  `main.go` étant hors zone, le previewer s'auto-initialise paresseusement (`sync.Once`) sans
  câblage au point d'assemblage.

## Golden strate 0 (avant édition)

```
BUILD_OK
VET_OK
ok  github.com/hazyhaar/horosvec-bench/cmd/hnbook-serve  (cached)
---GOFMT--- (aucune sortie)
```

## Gates (après édition)

```
BUILD_OK
VET_OK
ok  github.com/hazyhaar/horosvec-bench/cmd/hnbook-serve  1.161s
---GOFMT--- (aucune sortie)  GOFMT_DONE
```

## Sortie du test SSRF (décidable, obligatoire)

```
=== RUN   TestPreviewSSRFRefused
--- PASS: TestPreviewSSRFRefused (0.00s)
=== RUN   TestValidatePublicIP
--- PASS: TestValidatePublicIP (0.00s)
=== RUN   TestDialControlGuard
--- PASS: TestDialControlGuard (0.00s)
=== RUN   TestValidatePreviewURL
--- PASS: TestValidatePreviewURL (0.00s)
PASS
ok  github.com/hazyhaar/horosvec-bench/cmd/hnbook-serve  0.002s
```

`TestPreviewSSRFRefused` refuse (champs vides + error, aucun fetch abouti) :
`http://127.0.0.1:8472/`, `http://127.0.0.1/admin`, `http://169.254.169.254/latest/meta-data/`
(métadonnées cloud), `http://192.168.0.1/`, `http://10.0.0.5/`, `http://172.16.0.1/`,
`http://[::1]/`, `http://[fe80::1]/`, `http://0.0.0.0/`, `http://100.64.0.1/`,
`http://[fc00::1]/`, `ftp://example.com/`, `file:///etc/passwd`.

## Preuve d'embarquement

`strings hnbook-serve | grep -c "api/preview"` → **3** (> 0 : route + asset index.html embarqués).

## Choix de durcissement

1. Anti-SSRF à deux niveaux. Pré-vérification `validatePreviewURL` (schéma http/https seul,
   résolution DNS, refus si une IP résolue non publique) POUR une erreur claire tôt ; garde
   AUTORITAIRE `dialControlGuard` posée en `net.Dialer.Control` (via `Transport.DialContext`),
   rejouée au moment où la socket compose l'IP résolue — couvre le rebinding DNS entre la
   pré-résolution et le dial.
2. `validatePublicIP` : refuse loopback, lien-local unicast+multicast (dont 169.254.169.254),
   privées (10/8, 172.16/12, 192.168/16, fc00::/7 via `IsPrivate`), non spécifiée, multicast,
   plus plages explicites CGNAT 100.64/10, IETF 192.0.0/24, banc 198.18/15, réservé 240/4.
3. Redirections bornées à ≤ 3 (`CheckRedirect`), chaque saut re-validé par `validatePreviewURL`.
4. Bornes : contexte 5 s + `http.Client.Timeout` 5 s ; corps plafonné 512 Kio (`io.LimitReader`) ;
   analyse UNIQUEMENT si `Content-Type` commence par `text/html` ; statut non 200 → error ;
   User-Agent honnête `horosvec-demo-preview/1.0` ; keep-alive désactivé.
5. Parse OG stdlib : scan borné au `<head>`, balises `<meta>` à ordre d'attributs variable,
   guillemets simples/doubles, décodage `html.UnescapeString`, repli `<title>` + meta description.
6. Cache mémoire `url→(résultat, expiry)` TTL 1 h, borné à 2048 entrées (purge des expirées puis
   éviction simple), sous mutex.
7. Dégradation gracieuse : tout échec rend HTTP 200 + `{"error":..}` champs vides, jamais 500.
8. Anti-XSS front : titre/description/site en `textContent` ; `og:image` posée sur `img.src`
   après validation `new URL` de schéma http(s) ; aucun `innerHTML` de donnée distante.

## Negcriteria

N0 zone respectée (main.go NON touché — previewer paresseux). N1 zéro dépendance nouvelle
(x/net non ajouté). N2 /api/search, embedclient, moteur intacts. N3 SSRF prouvé par test
(y compris re-validation au connect). N4 zéro innerHTML distant, og:image validée. N5 zéro
secret. N6 zéro push/redéploiement. N7 dégradation gracieuse.
