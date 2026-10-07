# Backups

Each deployment's `backup.sh`, run nightly from cron, leaves a compressed database dump in
`/opt/goevent/backups`; see step 9 of the [standard](standard.md#9-backups) or
[tunnel](tunnel.md#9-backups) guide. That covers a bad migration or a mistaken delete. It
does not cover losing the server: the dumps are on the same disk as the database.

This guide copies them, and the rest of what the site cannot rebuild, somewhere else,
restores from them, and checks that restoring works.

## What `backup.sh` does

[`deploy/standard/backup.sh`](../../deploy/standard/backup.sh) (the tunnel copy is
identical):

- runs `pg_dump` inside the `postgres` container over its local socket, so no password is
  needed, and gzips it into `./backups` (or `BACKUP_DIR`) as
  `goevent-<UTC timestamp>.sql.gz`, readable only by its owner;
- writes to a `.partial` file first, and keeps it only if the dump ends with pg_dump's
  completion trailer, so a failed dump never looks like a backup;
- deletes dumps older than `KEEP_DAYS` (default 14), and only after a good one.

## What needs backing up

| What | Where it lives | How |
|---|---|---|
| The database: events, registrations, attendees, payments, accounts, queued email | Postgres's `pgdata` volume, dumped by `backup.sh` | Copy the dumps off the server |
| Event images | The R2 bucket (`BLOB_BUCKET`) | An independent copy |
| `.env`, above all `SECRET_KEY` | `/opt/goevent/.env` | A copy in your password manager |

**`SECRET_KEY` is as important as the database.** A restored database with a different key
still works as a site, but every manage link and ticket QR code already emailed stops
working, and email that was queued at backup time cannot be decrypted. Keep the key with
the backups' credentials, not on the server alone.

R2 is primary storage, not a backup against deletion. Keep copies under credentials the
running site cannot use.

Everything else (the image, Caddy's certificates, the tunnel) is recreated by following the
guide again.

## Copying off the server

The copies go to a private R2 bucket, with [rclone](https://rclone.org) run from its
container, so nothing is installed on the host. Any S3-compatible storage works the same
way with a different endpoint.

**Create the bucket and a token.** Under **R2 Object Storage**, create a bucket called
`goevent-backups` and leave public access **off**. Then **Manage API tokens**,
**Create API token**, with **Object Read & Write** on only that bucket. Use a token of its
own, not the image bucket's: a leaked site credential should not be able to delete the
backups.

**Retention** belongs to the bucket: add a lifecycle rule on the `db/` prefix deleting old
dumps after, say, 90 days. Do not apply it to `images/`, where an old object may still be an
event's current image.

**Give rclone the credentials** in a file of their own beside `.env`, at `0600`:

```sh
sudo install -m 600 /dev/null /opt/goevent/offsite.env
sudo nano /opt/goevent/offsite.env
```

```sh
RCLONE_CONFIG_OFFSITE_TYPE=s3
RCLONE_CONFIG_OFFSITE_PROVIDER=Cloudflare
RCLONE_CONFIG_OFFSITE_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
RCLONE_CONFIG_OFFSITE_ACCESS_KEY_ID=
RCLONE_CONFIG_OFFSITE_SECRET_ACCESS_KEY=
# The token can write to the bucket but not create it, so rclone must not try.
RCLONE_CONFIG_OFFSITE_NO_CHECK_BUCKET=true

# A separate read-only token scoped to the image bucket.
RCLONE_CONFIG_LIVE_TYPE=s3
RCLONE_CONFIG_LIVE_PROVIDER=Cloudflare
RCLONE_CONFIG_LIVE_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
RCLONE_CONFIG_LIVE_ACCESS_KEY_ID=
RCLONE_CONFIG_LIVE_SECRET_ACCESS_KEY=
RCLONE_CONFIG_LIVE_NO_CHECK_BUCKET=true
```

An `offsite` bucket in a different account or provider also protects against losing access
to the primary account.

**The copy script**, `/opt/goevent/offsite.sh`:

```bash
#!/usr/bin/env bash
# Copies what backup.sh and the site keep to the offsite bucket.
set -euo pipefail
cd "$(dirname "$0")"

rclone() {
  docker run --rm --env-file offsite.env \
    -v "$PWD/backups:/backups:ro" rclone/rclone:latest --quiet "$@"
}

rclone copy /backups offsite:goevent-backups/db
rclone copy live:goevent-images offsite:goevent-backups/images
```

Use your actual `BLOB_BUCKET` name. `copy` never deletes from the destination, so an image
deleted on the site is kept in the backup.

```sh
sudo chmod 700 /opt/goevent/offsite.sh
sudo /opt/goevent/offsite.sh
```

Then have cron run it after each successful backup, replacing the guide's line in
`sudo crontab -e`:

```crontab
15 3 * * * /opt/goevent/backup.sh && /opt/goevent/offsite.sh >>/var/log/goevent-backup.log 2>&1
```

## Restoring the database

On the server, in `/opt/goevent`. This **replaces** the current database with the dump, so
take one of the current state first:

```sh
sudo ./backup.sh
sudo docker compose stop server
sudo docker compose exec -T postgres dropdb -U goevent goevent
sudo docker compose exec -T postgres createdb -U goevent goevent
gunzip -c backups/goevent-<timestamp>.sql.gz \
  | sudo docker compose exec -T postgres psql -q -v ON_ERROR_STOP=1 -U goevent goevent >/dev/null
sudo docker compose start server
```

To restore on a new server, follow the guide up to starting the stack, with the **same
`SECRET_KEY`** in `.env`, then fetch the dump first:

```sh
sudo docker run --rm --env-file offsite.env -v "$PWD/backups:/backups" \
  rclone/rclone:latest --quiet copy offsite:goevent-backups/db/goevent-<timestamp>.sql.gz /backups
```

Restore images by reversing the copy (`copy offsite:goevent-backups/images live:goevent-images`)
with a temporary write-capable credential.

What to expect after a restore:

- Email jobs that were pending at backup time are sent again, possibly a second time.
  Delivery is at least once.
- Anything since the dump is gone: registrations, payments recorded by hand, check-ins. A
  gateway payment made in that window still exists at the gateway; reconcile it against the
  PayFast dashboard or SnapScan portal.

## Testing a restore

A backup that has never been restored is a hope. Every so often, restore the latest dump
into a scratch database beside the real one and look at it:

```sh
sudo docker compose exec -T postgres createdb -U goevent restore_check
gunzip -c "$(ls -t backups/goevent-*.sql.gz | head -1)" \
  | sudo docker compose exec -T postgres psql -q -v ON_ERROR_STOP=1 -U goevent restore_check >/dev/null
sudo docker compose exec -T postgres psql -U goevent restore_check -c 'select count(*) from registrations'
sudo docker compose exec -T postgres dropdb -U goevent restore_check
```

The live site is untouched throughout. Do the same with a dump fetched from the offsite
bucket now and then, since that is the copy you will need on the day the server is gone.

## What this does not give you

These are nightly snapshots: restoring loses up to a day. Point-in-time recovery needs
Postgres's write-ahead log shipped continuously, with a tool such as
[pgBackRest](https://pgbackrest.org) or [WAL-G](https://github.com/wal-g/wal-g), or a managed
database that does it for you. Worth considering before a large event, when a lost day of
registrations costs more than running it.
