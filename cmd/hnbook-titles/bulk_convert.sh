#!/usr/bin/env bash
# bulk_convert.sh — chargement massif parquet → SQLite via duckdb (voie deux-temps).
# Le schéma est créé par hnbook-titles init (source de vérité Go) ; duckdb n'insère que les lignes.
set -euo pipefail

DUCKDB="${DUCKDB:-/home/cl-ment/.local/bin/duckdb}"
PARQUET="${PARQUET:-/data/hackernews/hackernews.parquet}"

DB=""
LIMIT=0

usage() {
  cat <<'EOF'
usage: bulk_convert.sh --db <path> [--limit N]

  --db <path>    fichier SQLite cible (créé/initialisé par hnbook-titles init)
  --limit N      nombre max de lignes à importer (0 = tout le parquet ; défaut 0)

Environnement optionnel :
  DUCKDB   chemin du binaire duckdb (défaut /home/cl-ment/.local/bin/duckdb)
  PARQUET  chemin du parquet source (défaut /data/hackernews/hackernews.parquet)
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --db)
      DB="${2:?}"
      shift 2
      ;;
    --limit)
      LIMIT="${2:?}"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

if [[ -z "$DB" ]]; then
  echo "error: --db is required" >&2
  usage >&2
  exit 2
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BENCH_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
HNBOOK_TITLES="${HNBOOK_TITLES:-$BENCH_ROOT/bin/hnbook-titles}"

if [[ ! -x "$HNBOOK_TITLES" ]]; then
  (cd "$BENCH_ROOT" && env GOWORK=off CGO_ENABLED=0 go build -o "$HNBOOK_TITLES" ./cmd/hnbook-titles)
fi

"$HNBOOK_TITLES" init -db "$DB"

LIMIT_CLAUSE=""
if [[ "$LIMIT" -gt 0 ]]; then
  LIMIT_CLAUSE="LIMIT $LIMIT"
fi

# Échapper les quotes simples pour SQL.
DB_SQL="${DB//\'/\'\'}"

# DuckDB sqlite attach ne supporte pas INSERT OR REPLACE / ON CONFLICT sur table SQLite
# (Binder Error). Post-init la table est vide : INSERT simple = chargement one-shot équivalent.
"$DUCKDB" -unsigned <<SQL
INSTALL sqlite;
LOAD sqlite;
ATTACH '$DB_SQL' AS t (TYPE SQLITE);
INSERT INTO t.item (id, ts, type, title, parent, text)
SELECT id, time AS ts, CAST(type AS VARCHAR), CAST(title AS VARCHAR), parent,
       CAST(text AS VARCHAR)
FROM read_parquet('$PARQUET')
WHERE title IS NOT NULL
$LIMIT_CLAUSE;
SQL

echo "bulk_convert: done db=$DB limit=$LIMIT"