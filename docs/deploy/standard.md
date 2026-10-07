# Deploying: standard

From an empty server to a running site: goevent and Postgres under Docker Compose, Caddy
in front with a Let's Encrypt certificate, event images in Cloudflare R2, and mail through
any SMTP provider. It uses [`deploy/standard`](../../deploy/standard), which holds
`compose.yaml`, `Caddyfile`, `.env.example` and `backup.sh`.

## 1. What you need

- **A server** with a public IPv4 address, any VPS running a current Ubuntu or Debian. One
  vCPU and 1 GB of memory is enough to start. SSH access and `sudo`.
- **A domain**, and a hostname on it for the site (say `events.example.com`).
- **A Cloudflare account** for R2, where event images live. The free tier covers a typical
  organisation.
- **An SMTP provider**: host, port, username, password, and an address it lets you send
  from. The server refuses to start without mail, because tickets are emailed.
- **A published image.** A release from
  [GHCR](https://github.com/17xande-dev/goevent/pkgs/container/goevent), or your own; see
  [Publishing an image](README.md#publishing-an-image).
- **Optionally, a payment gateway account**: PayFast, SnapScan, or both. Without one the
  site runs free events and pay-later (EFT or cash) events.

## 2. Prepare the server

Install Docker Engine and the Compose plugin following
[Docker's instructions for your distribution](https://docs.docker.com/engine/install/).
`docker compose version` should print a version.

Allow SSH, HTTP and HTTPS in, and nothing else. With `ufw`:

```sh
sudo ufw allow OpenSSH && sudo ufw allow 80/tcp && sudo ufw allow 443 && sudo ufw enable
```

Caddy is the only container that publishes ports (80, 443 and 443/udp). Postgres and the
server are reachable only from inside the stack.

Port 443 must stay open to the whole internet, not just to your visitors' networks:
PayFast's and SnapScan's servers post payment notifications to it.

## 3. Point the domain at the server

Create an **A record** for `events.example.com` with the server's address. Caddy asks Let's
Encrypt for a certificate on first start, which only succeeds once the name resolves here;
check with `dig +short events.example.com`.

If the domain is on Cloudflare, make this record **DNS only** (grey cloud). Proxied, every
request would arrive from Cloudflare with the client's address in a header this deployment
does not read (`CLIENT_IP_SOURCE` is `forwarded`, which trusts Caddy's `X-Forwarded-For`).
The [tunnel deployment](tunnel.md) is the one built for Cloudflare in front.

## 4. Create the R2 bucket

In the Cloudflare dashboard, under **R2 Object Storage**:

1. **Create bucket**, called `goevent-images`.
2. Open it, then **Settings**, **Public access**, **Custom Domains**, **Connect domain**,
   and give it a hostname such as `images.example.com` (on your Cloudflare account). That
   address is `BLOB_PUBLIC_BASE_URL`. The `r2.dev` address also works, but Cloudflare
   rate-limits it and says it is not for production.
3. Back on the R2 overview, **Manage API tokens**, **Create API token**, with **Object Read
   & Write** on **only this bucket**. Copy the access key id, the secret access key, and the
   endpoint `https://<account-id>.r2.cloudflarestorage.com` now; the secret is shown once.

## 5. Copy the deployment to the server

Take `deploy/standard` from the same release as the image you will run:

```sh
git clone --depth 1 --branch v1.0.0 https://github.com/17xande-dev/goevent.git /tmp/goevent
sudo cp -r /tmp/goevent/deploy/standard /opt/goevent
cd /opt/goevent
sudo cp .env.example .env && sudo chmod 600 .env
```

## 6. Fill in `.env`

Open `.env` (`sudo nano .env`). Each value is explained beside it; in short:

| Setting | Value |
|---|---|
| `GOEVENT_VERSION` | the release you are running, e.g. `v1.0.0`. Never `latest` |
| `DOMAIN` | `events.example.com` |
| `ACME_EMAIL` | where Let's Encrypt writes about a certificate it cannot renew |
| `SITE_NAME` | your organisation's name, shown in the header, titles and emails |
| `POSTGRES_PASSWORD` | the output of `openssl rand -hex 32` |
| `SECRET_KEY` | another `openssl rand -hex 32`. Keep a copy somewhere safe; see below |
| `PAYFAST_SANDBOX`, `PAYFAST_*` | leave the sandbox values for now; see [Going live](#going-live). To run without PayFast, delete the three `PAYFAST_MERCHANT_ID`/`KEY`/`PASSPHRASE` lines, but keep `PAYFAST_SANDBOX` set: Compose requires it |
| `BLOB_ENDPOINT` | the R2 endpoint from step 4, **without** `https://` |
| `BLOB_ACCESS_KEY_ID`, `BLOB_SECRET_ACCESS_KEY` | the token from step 4 |
| `BLOB_PUBLIC_BASE_URL` | `https://images.example.com` |
| `SMTP_*`, `EMAIL_FROM` | from your mail provider |
| `NOTIFY_EMAIL` | optional: who gets a copy of each confirmed registration |

`SECRET_KEY` signs every manage link and ticket QR code the site emails. Lose it or change
it and every ticket already sent stops scanning. Store it with your other secrets, apart
from the server.

SnapScan, or any other setting from [Configuration](../configuration.md), is added to the
same file; `.env.example` at the repository root documents each one.

### Optional: a theme

Put a [theme](../theming.md) in a `theme/` directory next to `compose.yaml`
(`sudo git clone <your-theme-repo> theme`) and uncomment `TEMPLATE_DIR` and `STATIC_DIR` in
`.env`. Use the `/theme/...` paths given there, not host paths: the server only sees the
mounted directory. See [Using a theme in a deployment](../theming.md#using-a-theme-in-a-deployment).

## 7. Start it

```sh
sudo docker compose up -d
sudo docker compose ps          # postgres healthy; server and caddy up
curl https://events.example.com/healthz     # -> ok
```

The first start takes a minute: images download, migrations run, and Caddy obtains its
certificate. If `/healthz` does not answer, `sudo docker compose logs server` names any
setting the server refused, and `sudo docker compose logs caddy` says why a certificate
could not be issued.

## 8. Claim the admin account

With no account yet, the server logged a one-time setup token when it started:

```sh
sudo docker compose logs server | grep setup_token
```

Open `https://events.example.com/admin`, which leads to `/admin/setup`, paste the token, and
choose an address and password. That account is the `owner`, and the token is spent. See
[Admin and accounts](../admin.md).

## 9. Backups

`backup.sh` writes a compressed dump of the database to `/opt/goevent/backups` and keeps 14
days of them (`KEEP_DAYS` changes that). Run it once, then schedule it in root's crontab
(`sudo crontab -e`):

```sh
sudo /opt/goevent/backup.sh
```

```crontab
15 3 * * * /opt/goevent/backup.sh >>/var/log/goevent-backup.log 2>&1
```

It refuses to keep a dump that did not finish, and prunes old ones only after a good one.
These copies live on the same disk as the database; [Backups](backups.md) copies them
elsewhere and covers restoring.

## Day two

**Updating.** Take a backup, set `GOEVENT_VERSION` in `.env` to the new release, then:

```sh
sudo ./backup.sh
sudo docker compose pull && sudo docker compose up -d
```

Migrations run as the new version starts. Read the release notes first: a release that
changes this directory's files says so, and you copy those over as in step 5.

### Changing a setting or a secret

Edit `.env` and run `sudo docker compose up -d`; containers whose settings changed are
recreated.

`POSTGRES_PASSWORD` is the exception: Postgres reads it only when the database is first
created. Change it in the database first, then in `.env`:

```sh
sudo docker compose exec postgres psql -U goevent -c "ALTER USER goevent PASSWORD '<new>'"
```

`SECRET_KEY` should not be changed at all unless it has leaked. Changing it invalidates
every manage link and ticket already emailed and makes queued email undecryptable; see
[Configuration](../configuration.md#secret_key).

## Going live

The site starts on PayFast's sandbox, which takes no real money. When ready, replace
`PAYFAST_MERCHANT_ID`, `PAYFAST_MERCHANT_KEY` and `PAYFAST_PASSPHRASE` with your live
values, set `PAYFAST_SANDBOX=false`, and `sudo docker compose up -d`. The server refuses
`PAYFAST_SANDBOX=false` with the sandbox merchant id. See
[Going live](../payments.md#going-live).

For SnapScan, give SnapScan support `https://events.example.com/payments/snapscan/callback`
as the webhook address. SnapScan has no sandbox.

## When a step fails

| Symptom | Likely cause |
|---|---|
| `docker compose` says a variable is not set | That setting is empty or missing in `.env`; the message names it |
| The server container keeps restarting | It refused a setting; `sudo docker compose logs server` names it |
| No certificate; the browser warns | DNS does not point here yet, the record is proxied through Cloudflare, or 80/443 are blocked |
| Images upload but do not display | `BLOB_PUBLIC_BASE_URL` is not the bucket's public address, or public access is off |
| Payments go through but registrations stay pending | The gateway cannot reach `/payments/{gateway}/callback`, or the server rejected it; `logs server` shows `rejected payment callback` with the reason |
| `pull access denied` for the image | The GHCR package is private, or `GOEVENT_VERSION` names a tag that does not exist |
