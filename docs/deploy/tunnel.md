# Deploying: tunnel

From an empty server to a running site reached through a Cloudflare Tunnel: goevent and
Postgres under Docker Compose, `cloudflared` carrying requests in, event images in R2, and
mail through Microsoft Graph. It uses [`deploy/tunnel`](../../deploy/tunnel), which holds
`compose.yaml`, `.env.example` and `backup.sh`.

`cloudflared` dials out to Cloudflare, so the server publishes **no ports at all**: no port
forward, no public address, no certificate to renew. That makes it the deployment for a
machine behind a home or office router, and a sound one for a VPS you would rather keep
closed.

## 1. What you need

- **A server** running a current Ubuntu or Debian, with outbound internet access. A VM on a
  home server is fine. One vCPU and 1 GB of memory is enough to start. SSH access and
  `sudo`.
- **A domain on Cloudflare**, and two hostnames on it: one for the site (say
  `events.example.com`) and one for event images (`images.example.com`).
- **Microsoft 365**, and permission to register an app in its Entra tenant, or an admin who
  will. Mail is required, because tickets are emailed.
- **An R2 bucket** for images. Follow [the R2 setup](standard.md#4-create-the-r2-bucket).
- **A published image.** A release from
  [GHCR](https://github.com/17xande-dev/goevent/pkgs/container/goevent), or your own; see
  [Publishing an image](README.md#publishing-an-image).
- **Optionally, a payment gateway account**: PayFast, SnapScan, or both.

## 2. Prepare the server

Install Docker Engine and the Compose plugin following
[Docker's instructions for your distribution](https://docs.docker.com/engine/install/).
Nothing needs opening in a firewall; it can refuse every inbound connection except your SSH.

## 3. Register the mail app

In the Entra admin center, register an application, give it the **`Mail.Send` application
permission** on Microsoft Graph with admin consent, and create a client secret.
[Microsoft Exchange Online](../email.md#microsoft-exchange-online) covers what the server
needs. On its own `Mail.Send` lets the app send as any mailbox in the tenant, so restrict it
to the one it sends as with Exchange Online's RBAC for Applications; check Microsoft's
current documentation for the steps.

Keep three values: the **tenant id**, the **client id**, and the **client secret**, which is
shown once.

## 4. Create the tunnel

In the Cloudflare dashboard, open **Zero Trust**, **Networks**, **Tunnels**,
**Create a tunnel**:

1. Choose **Cloudflared**, and name it (say `goevent`).
2. On the install page, pick **Docker**. The command shown ends in `--token eyJ…`; copy just
   that token. It is `TUNNEL_TOKEN`, the tunnel's whole credential. Do not run the command;
   the stack runs `cloudflared` itself.
3. Add the site's **public hostname** (newer dashboards call it a *published application
   route*):

   | Hostname | Service |
   |---|---|
   | `events.example.com` | `HTTP` to `server:8080` |

   `server` is the container's name inside the stack. Images use the custom domain
   connected to the R2 bucket, not a tunnel route. Cloudflare creates the DNS record; remove
   any conflicting record first.

**Do not put Cloudflare Access, or any other login, in front of the whole hostname.**
PayFast and SnapScan post payment notifications to `/payments/payfast/callback` and
`/payments/snapscan/callback` on it, and they cannot sign in. Registrants also need the
public pages. If you protect `/admin` with Access, scope the policy to that path.

`CLIENT_IP_SOURCE` is fixed to `cloudflare` in `compose.yaml`, which reads
`CF-Connecting-IP`. That is safe here because the tunnel is the only way in; PayFast's
source-IP check and the rate limits depend on it.

## 5. Copy the deployment to the server

Take `deploy/tunnel` from the same release as the image you will run:

```sh
git clone --depth 1 --branch v1.0.0 https://github.com/17xande-dev/goevent.git /tmp/goevent
sudo cp -r /tmp/goevent/deploy/tunnel /opt/goevent
cd /opt/goevent
sudo cp .env.example .env && sudo chmod 600 .env
```

## 6. Fill in `.env`

Open `.env` (`sudo nano .env`). Each value is explained beside it; in short:

| Setting | Value |
|---|---|
| `GOEVENT_VERSION` | the release you are running, e.g. `v1.0.0`. Never `latest` |
| `TUNNEL_TOKEN` | the token from step 4 |
| `DOMAIN` | the site hostname from step 4 |
| `SITE_NAME` | your organisation's name |
| `POSTGRES_PASSWORD`, `SECRET_KEY` | `openssl rand -hex 32`, once for each. Keep a copy of `SECRET_KEY` apart from the server |
| `PAYFAST_SANDBOX`, `PAYFAST_*` | leave the sandbox values for now; see [Going live](#going-live). To run without PayFast, delete the three merchant lines and keep `PAYFAST_SANDBOX` |
| `GRAPH_TENANT_ID`, `GRAPH_CLIENT_ID`, `GRAPH_CLIENT_SECRET` | from step 3 |
| `EMAIL_FROM` | the mailbox the app sends as |
| `NOTIFY_EMAIL` | optional: who gets a copy of each confirmed registration |
| `BLOB_*` | the R2 bucket and its token |

`SECRET_KEY` signs every manage link and ticket QR code the site emails; changing it stops
every ticket already sent from scanning. See [Configuration](../configuration.md#secret_key).

### Optional: a theme

Put a [theme](../theming.md) in a `theme/` directory next to `compose.yaml` and uncomment
`TEMPLATE_DIR` and `STATIC_DIR` in `.env`, using the `/theme/...` paths given there. See
[Using a theme in a deployment](../theming.md#using-a-theme-in-a-deployment).

## 7. Start it

```sh
sudo docker compose up -d
sudo docker compose ps -a       # postgres healthy; server and cloudflared up
curl https://events.example.com/healthz     # -> ok
```

The tunnel's page in the dashboard should show it **Healthy**. If `/healthz` does not
answer, `sudo docker compose logs server` names any setting the server refused, and
`sudo docker compose logs cloudflared` shows whether the tunnel connected.

## 8. Claim the admin account

```sh
sudo docker compose logs server | grep setup_token
```

Open `https://events.example.com/admin`, which leads to `/admin/setup`, paste the token, and
choose an address and password. That account is the `owner`. See
[Admin and accounts](../admin.md).

## 9. Backups

`backup.sh` writes a compressed dump of the database to `/opt/goevent/backups` and keeps 14
days (`KEEP_DAYS` changes that). Run it once, then schedule it in root's crontab:

```sh
sudo /opt/goevent/backup.sh
```

```crontab
15 3 * * * /opt/goevent/backup.sh >>/var/log/goevent-backup.log 2>&1
```

Event images are not in the dump; they are in R2. [Backups](backups.md) covers independent
copies and restoring.

## Day two

**Updating.** Take a backup, set `GOEVENT_VERSION` in `.env` to the new release, then:

```sh
sudo ./backup.sh
sudo docker compose pull && sudo docker compose up -d
```

**Changing a setting or a secret.** Edit `.env` and `sudo docker compose up -d`. For
`POSTGRES_PASSWORD` and `SECRET_KEY`, see
[Changing a setting or a secret](standard.md#changing-a-setting-or-a-secret).

A new Graph client secret (they expire, two years at most) is an ordinary change: create it
in Entra, put it in `.env`, `up -d`. Until you do, confirmations queue and retry rather than
being lost.

`cloudflared` runs the `latest` image; `sudo docker compose pull` updates it with the rest.

## Going live

The site starts on PayFast's sandbox. When ready, replace `PAYFAST_MERCHANT_ID`,
`PAYFAST_MERCHANT_KEY` and `PAYFAST_PASSPHRASE` with your live values, set
`PAYFAST_SANDBOX=false`, and `sudo docker compose up -d`. See
[Going live](../payments.md#going-live). For SnapScan, give support
`https://events.example.com/payments/snapscan/callback` as the webhook address.

## When a step fails

| Symptom | Likely cause |
|---|---|
| `docker compose` says a variable is not set | That setting is empty or missing in `.env`; the message names it |
| The server container keeps restarting | It refused a setting; `sudo docker compose logs server` names it |
| The tunnel shows *Down* or *Inactive* | `TUNNEL_TOKEN` is wrong or truncated, or outbound traffic is blocked; see `logs cloudflared` |
| Cloudflare shows error 502 | The hostname's service must be `server:8080`, over `HTTP` |
| Images upload but do not display | The R2 custom domain or `BLOB_PUBLIC_BASE_URL` is wrong |
| Mail fails | `Mail.Send` lacks admin consent, or the app is not allowed that mailbox. The registration's email list in the admin shows the failed attempts |
| Payments go through but registrations stay pending | Something in front of the hostname blocks the gateway's notification, or the server rejected it; `logs server` shows `rejected payment callback` with the reason |
