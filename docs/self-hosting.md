# Self-hosting Raises

Raises uses one SQLite database, so run one replica only. Put it behind your HTTPS reverse proxy and keep the Docker volume on durable storage.

## Start

```sh
git clone https://github.com/raisestracker/raises.git
cd raises
cp .env.example .env
docker compose build
docker compose up -d
curl http://127.0.0.1:8080/healthz
curl http://127.0.0.1:8080/readyz
```

The Compose file builds locally, binds only `127.0.0.1:8080`, and stores SQLite at `/data/raises.db` in the `raises_data` volume. Configure an HTTPS reverse proxy for your chosen hostname and set `BASE_URL=https://raises.example.com` in `.env`.

Set `RAISES_URL=https://raises.example.com` in Rails applications that should send to this server.

## GitHub App

GitHub App support is optional. Leave every GitHub App variable blank for local health checks. When enabled, set all of `GITHUB_APP_ID`, `GITHUB_APP_CLIENT_ID`, `GITHUB_APP_CLIENT_SECRET`, `GITHUB_APP_PRIVATE_KEY`, and `GITHUB_APP_WEBHOOK_SECRET`.

For a server at `https://raises.example.com`, set these GitHub App URLs:

- Homepage: `https://raises.example.com`
- Callback: `https://raises.example.com/auth/github/callback`
- Setup: `https://raises.example.com/github/setup`
- Webhook: `https://raises.example.com/webhooks/github`

Grant **Metadata: read** and **Issues: read and write** permissions. Subscribe to **Installation** and **Installation repositories** events. Transport the private key as base64 text when an environment variable cannot preserve newlines; Raises accepts either PEM or base64-encoded PEM.

## Backup and upgrade

Stop the service before copying the volume so SQLite and its WAL files are consistent:

```sh
docker compose stop
docker run --rm -v raises_raises_data:/data -v "$PWD":/backup alpine tar czf /backup/raises-backup.tgz /data
docker compose start
```

For an upgrade, pull the new source, review `.env`, rebuild, and restart:

```sh
git pull origin master
docker compose build
docker compose up -d
```

## Optional integrations

Set `REPORT_ENABLED=1` or `OPERATIONAL_ALERTS_ENABLED=1` only after setting `REPORT_EMAIL_FROM`, `REPORT_EMAIL_TO`, and `AWS_REGION` for Amazon SES. Provide AWS credentials through `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optional `AWS_SESSION_TOKEN`, or use an attached instance role. Do not enable SES by default.

To enable ntfy on a fresh install, start with `NTFY_ENABLED=0` and sign in once so the intended GitHub account exists locally. Then set `INITIAL_OWNER_GITHUB_ID` to that account's numeric GitHub user ID, add `NTFY_TOPIC` and `NTFY_TOKEN`, set `NTFY_ENABLED=1`, and restart Raises. Startup fails if that owner ID does not resolve to the signed-in account. `WEBHOOK_ENCRYPTION_KEY` encrypts stored outbound webhook URLs and signing secrets.
