# G1 — Les trois inconnues serveur mesurées (tâche 019f5bb1-54ff, projet proj_horosvec_poc)

Date : 2026-07-13, 14:01–14:30 UTC. Machine mesurée : horos-prod (37.187.150.79),
Xeon E5-1650 v4 @ 3,60 GHz, 12 threads, 64 190 Mio RAM. Binaire : `hnbook-build`
compilé localement `GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=amd64` depuis
`/devhoros/horosvec-bench` (HEAD du 2026-07-13), poussé avec l'arène
`data/arenas/prefix11600.arena` (+ `.ids`) — le MÊME intrant exact que la baseline
locale du minichrono 2026-07-10 (préfixe du corpus HN réel, 11 600 vecteurs fp16
dim 512), ce qui rend le ratio propre (S-G1.3 : provenance constatée).
Répertoire de travail serveur : `/home/ubuntu/g1-mesures/` (script `run.sh`,
logs `build_N.log`, échantillons `lat_*.txt`). Toute invocation préfixée
`cd /home/ubuntu/g1-mesures` (C-G1.4).

## Inconnue 1 — temps de build shard-jour (C-G1.1, 3 runs)

| run | wall (time -v) | secondes |
|---|---|---|
| 1 | 5:07.80 | 307,8 |
| 2 | 5:09.73 | 309,7 |
| 3 | 5:12.08 | 312,1 |

Moyenne 309,9 s. **Ratio à la baseline locale 32 cœurs (65,08 s) : ×4,76.**
Verdict de bande : la bande extrapolée « 2-3× plus lent » est **fausse** — le box
est ~4,8× plus lent (6 cœurs physiques à 3,6 GHz contre 32, et le build horosvec
n'exploite qu'~40 % des cœurs par défaut). Verdict d'exploitation : **tient** —
~5 min 10 par shard-jour reste trivial en cadence nocturne. Extrapolation
shard-mois à ce ratio : 2 192,6 s × 4,76 ≈ **2 h 54** (S-G1.1 : build mensuel réel
non exécuté — budget temps — cette valeur est une EXTRAPOLATION étiquetée comme
telle ; la publication mensuelle de G4 doit prévoir une fenêtre nocturne dédiée).

## Inconnue 2 — RAM de pointe (C-G1.2, time -v « Maximum resident set size »)

58 080 / 57 444 / 57 928 Ko sur les trois runs (~56,6 Mio), conforme à la baseline
locale (56 956 Ko). Rapportée aux 64 Gio du box (47 Gio disponibles au moment du
run) : **tient très largement** ; l'extrapolation shard-mois (~836 Mio mesurés en
local) tient aussi sans réserve.

## Inconnue 3 — contention avec la démo vivante (C-G1.3, profil identique)

Profil étalon (S-G1.2, levé) : `GET /api/search?q=rust+memory+safety&k=10` en
localhost sur :8472, `curl -w %{time_total}` — le chemin complet embed (:8471,
~97 ms mesuré dans la réponse JSON) + recherche (~2 ms). Échantillons bruts dans
`lat_idle*.txt` / `lat_during_*.txt` (11 à vide, 181 pendant les trois builds).

| régime | n | p50 | p95 | min | max |
|---|---|---|---|---|---|
| sans build | 11 | 92,8 ms | 99,7 ms | 91,5 | 99,7 |
| build concurrent | 181 | 120,7 ms | 127,3 ms | 90,7 | 147,4 |

**Delta = seuil C6 de la tâche mère : p50 +27,9 ms (+30 %), p95 +27,6 ms (+28 %).**
Lecture : la contention frappe surtout le sidecar d'embedding (pur CPU, en
concurrence avec les ~5 cœurs du build) ; la démo reste parfaitement servie
(<150 ms au pire échantillon). Attribution mesurée en face, pas par élimination
(C-G1.8) : l'`embed_ms` rapporté par l'API domine la latence dans les deux régimes.

## Étanchéité (C-G1.5, C-G1.6)

- Index servi inchangé : mtime `hnbook_index.db` 2026-07-08 23:51, `hnbook.arena`
  2026-07-08 05:44, identiques avant/après (les builds écrivent `/tmp/g1_shard_N.db`).
- Lib horosvec intouchée : `git status` du dépôt ne montre que l'audit non suivi
  préexistant du 13.
- Aucune automation créée : zéro timer systemd hn/vec, crontab inchangé
  (1 entrée préexistante rotate-traces).

## Chiffres à retenir (gravés en reminder proj_horosvec_incremental)

1. Build shard-jour sur horos-prod : **~310 s (×4,76 vs local)** ; shard-mois
   extrapolé ~2 h 54 (non mesuré).
2. RAM de pointe build shard-jour : **~57 Mo** (64 Gio disponibles — non-sujet).
3. Seuil de contention C6 : **+28 % sur le p95** de /api/search (99,7 → 127,3 ms)
   pendant un build concurrent, profil étalon localhost.
