# hnbook-titles — magasin SQLite de titres HackerNews

Binaire Go pur (driver `modernc.org/sqlite`, sans CGO) qui gère une base SQLite de titres
pour la démo `hnbook-serve`. Architecture **voie deux-temps** :

1. **Chargement massif (one-shot, opérateur)** — le parquet réel (`/data/hackernews/hackernews.parquet`)
   n'est jamais lu par le binaire Go. `bulk_convert.sh` appelle `duckdb` pour remplir la base.
2. **Delta (runtime serveur)** — `append` ingère un flux NDJSON (`INSERT OR REPLACE` par lots).

Le schéma (`item(id, ts, type, title)`) est défini uniquement par `hnbook-titles init`.
Le corps (`text`) est volontairement omis (seul le titre est servi).

## Schéma

```sql
CREATE TABLE IF NOT EXISTS item(
  id INTEGER PRIMARY KEY,
  ts INTEGER,
  type TEXT,
  title TEXT
);
```

PRAGMA : `journal_mode=WAL`, `busy_timeout=5000`.

## Commandes

```bash
# Créer / initialiser la base (idempotent)
hnbook-titles init -db /path/to/hn_titles.db

# Charger depuis le parquet (local, one-shot — puis rsync du .db au serveur)
./bulk_convert.sh --db /path/to/hn_titles.db
# Tranche de test (ex. 200k lignes) :
./bulk_convert.sh --db /tmp/probe.db --limit 200000

# Delta : append NDJSON (une ligne JSON par item)
echo '{"id":42,"ts":1160418091,"type":"story","title":"..."}' | \
  hnbook-titles append -db /path/to/hn_titles.db -in -

# Statistiques
hnbook-titles stat -db /path/to/hn_titles.db
```

## Flux opérateur typique

```
init → bulk_convert.sh (machine locale, parquet 28M) → rsync .db → serveur
                                                      ↘ append (delta G4, hors scope ici)
```

Le run complet **28M lignes** est un geste opérateur long (hors mission de développement) :
il s'exécute une fois en local, puis le fichier `.db` est copié sur le serveur.
Le binaire déployé ne lit jamais de parquet.

**Note duckdb** : l'extension sqlite de duckdb ne supporte pas `INSERT OR REPLACE` ni `ON CONFLICT`
sur une base attachée. `bulk_convert.sh` utilise `INSERT` simple après `init` (table vide) ;
pour un rechargement complet, supprimer le `.db` ou repasser par `init` sur fichier neuf.

## Build

```bash
cd /devhoros/horosvec-bench
env GOWORK=off CGO_ENABLED=0 go build ./cmd/hnbook-titles
```