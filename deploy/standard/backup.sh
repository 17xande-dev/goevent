#!/usr/bin/env bash
# A compressed pg_dump into ./backups beside this file, keeping KEEP_DAYS days
# (default 14). Run nightly from cron — see "Backups" in the deploy guide — and
# by hand before anything risky.
#
# This is a copy on the same disk as the database: it covers a bad migration or
# a mistaken delete, not losing the server. docs/deploy/backups.md copies it
# somewhere else.
#
# bash for pipefail: without it `pg_dump | gzip` reports only gzip's status, so
# a failed pg_dump yields a valid, empty gzip that looks like a backup.
set -euo pipefail
umask 077

cd "$(dirname "$0")"
dir=${BACKUP_DIR:-./backups}
keep=${KEEP_DAYS:-14}
mkdir -p "$dir"

ts=$(date -u +%Y%m%dT%H%M%SZ)
part="$dir/.goevent-$ts.sql.gz.partial"
trap 'rm -f "$part"' EXIT

# Inside the postgres container, over its own socket, which the official image
# trusts for local connections — so no password is needed here.
docker compose exec -T postgres pg_dump -U goevent goevent | gzip >"$part"

# pg_dump writes this trailer only when a dump finished. It is not the last
# line — since 17.6 a `\unrestrict` line follows it — so look near the end.
# Captured rather than piped to `grep -q`, which would stop reading at the
# match and fail the pipeline with SIGPIPE.
trailer=$(gunzip -c "$part" | tail -n 20)
case "$trailer" in
  *"PostgreSQL database dump complete"*) ;;
  *)
    echo "backup: pg_dump output is incomplete; keeping the older backups untouched" >&2
    exit 1
    ;;
esac

mv "$part" "$dir/goevent-$ts.sql.gz"
# Only after a verified dump, so a run of bad nights never erodes the good ones.
find "$dir" -maxdepth 1 -name 'goevent-*.sql.gz' -mtime +"$keep" -delete
echo "$dir/goevent-$ts.sql.gz"
