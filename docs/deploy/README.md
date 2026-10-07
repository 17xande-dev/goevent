# Deploying

goevent runs as a small Docker Compose stack on one server. There are two ready-made
deployments; each is a directory in [`deploy/`](../../deploy) that you copy to the server,
fill in one `.env`, and start with `docker compose up -d`.

| | [Standard](standard.md) | [Tunnel](tunnel.md) |
|---|---|---|
| **Directory** | [`deploy/standard`](../../deploy/standard) | [`deploy/tunnel`](../../deploy/tunnel) |
| **How requests get in** | Caddy, with a Let's Encrypt certificate, on ports 80 and 443 | A Cloudflare Tunnel; no inbound ports at all |
| **`CLIENT_IP_SOURCE`** | `forwarded` (set in `compose.yaml`) | `cloudflare` (set in `compose.yaml`) |
| **Event images** | A public Cloudflare R2 bucket | A public Cloudflare R2 bucket |
| **Mail** | Any SMTP provider | Microsoft 365, through Microsoft Graph |
| **Pick it when** | You have a server with a public address. The default | The server has no public address (a home lab, an office), or you would rather open no ports; your domain is on Cloudflare; your mail is Microsoft 365 |

Both run Postgres 17 beside the server, from the published image
`ghcr.io/17xande-dev/goevent:${GOEVENT_VERSION}`, and both include a `backup.sh` for a
nightly database dump to the server's own disk. [Backups](backups.md) copies those off it
and covers restoring.

## Payment notifications must reach the server

PayFast's ITN and SnapScan's webhook are requests from the gateway's servers to yours:

- PayFast posts to `https://<DOMAIN>/payments/payfast/callback` (or `PAYFAST_NOTIFY_URL`).
- SnapScan posts to `https://<DOMAIN>/payments/snapscan/callback`, an address SnapScan
  support sets on your merchant account.

Both have to be reachable from the internet. In the standard deployment that is Caddy on
443; in the tunnel deployment it is the tunnel's public hostname. A firewall, an access
policy such as Cloudflare Access in front of the hostname, or a server only reachable on a
private network will let people pay and leave their registrations pending. See
[Payments](../payments.md).

## The `.env`

Everything a deployment needs, credentials included, goes in the `.env` beside its
`compose.yaml`, readable by you alone:

```sh
cp .env.example .env && chmod 600 .env
```

Each directory's `.env.example` holds only what that deployment needs, and `compose.yaml`
derives what it can, so values that must agree cannot disagree:

- `BASE_URL` is `https://${DOMAIN}`;
- `DATABASE_URL` is built from `POSTGRES_PASSWORD`;
- `CLIENT_IP_SOURCE` is fixed to match what is in front of the server;
- `PAYFAST_SANDBOX` has no default: Compose refuses to start until `.env` says `true` or
  `false`.

Every other setting is in [Configuration](../configuration.md) and can be added to the same
file. Credentials can arrive as files instead; see
[Where configuration and secrets live](../configuration.md#where-configuration-and-secrets-live).

**`SECRET_KEY` must be generated (`openssl rand -hex 32`) and kept.** It signs every
manage link and ticket QR code you email and encrypts queued email; changing it
invalidates all of them. Back it up with the database.

## Publishing an image

The image is built on a workstation and pushed to GHCR by hand with `make publish`, from a
clean checkout of a version tag (`git tag v1.2.3 && git push --tags`). There is no CI
workflow that publishes. `make publish` refuses a dirty tree or an untagged commit, and
pushes both the tag and `latest`.

GHCR makes a newly pushed package private. Set it public in the package's settings after
the first publish, or the server cannot pull it without a token.

## Running it anywhere else

The binary is static and the image is distroless and runs as a non-root user, so it runs
anywhere a container does. It reads `PORT` from the environment, takes a single
`DATABASE_URL`, logs JSON to stdout, and serves `GET /healthz`.

`-migrate` and `-migrate-status` read `DATABASE_URL` and nothing else, so a migration job
need not be trusted with the payment and mail secrets. `-check-config` validates the whole
environment without touching anything; run it before `-migrate`:

```sh
goevent -check-config && goevent -migrate && exec goevent
```

Behind a proxy, set `CLIENT_IP_SOURCE` to match it: PayFast's source-IP check and the rate
limits both depend on the client address being real.

`IMAGE_DIR` stores event images on local disk and suits a single instance with a persistent
volume. The Compose deployments use R2 instead and mount no image volume; switching one of
them to `IMAGE_DIR` needs a volume added for that path, or images are lost when the
container is recreated. Anything with more than one instance needs the `BLOB_*` settings.
