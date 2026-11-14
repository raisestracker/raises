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

Grant **Metadata: read** and **Issues: read and write** permissions. Subscribe to **Installation**, **Installation repositories** and **Issues** events. The Issues subscription marks an error fixed when its GitHub issue is closed. Transport the private key as base64 text when an environment variable cannot preserve newlines; Raises accepts either PEM or base64-encoded PEM.

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

The service runs as the unprivileged uid `65532`, and the image ships `/data` owned by that uid, so a fresh volume works without extra steps. A volume created by an older root-run image is still root-owned and the new container cannot write to it: give it to the runtime uid once before starting the upgraded container.

```sh
docker compose stop
docker run --rm -v raises_raises_data:/data busybox chown -R 65532:65532 /data
docker compose build
docker compose up -d
curl http://127.0.0.1:8080/readyz
```

Rollback is the previous image: `docker compose stop`, point the build back at the previous commit (or retag), `docker compose up -d`. The data files stay compatible in both directions; no dump or migration is involved.

## Optional integrations

Set `REPORT_ENABLED=1` or `OPERATIONAL_ALERTS_ENABLED=1` only after setting `REPORT_EMAIL_FROM`, `REPORT_EMAIL_TO`, and `AWS_REGION` for Amazon SES. Provide AWS credentials through `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, and optional `AWS_SESSION_TOKEN`, or use an attached instance role. Do not enable SES by default.

To enable ntfy on a fresh install, start with `NTFY_ENABLED=0` and sign in once so the intended GitHub account exists locally. Then set `INITIAL_OWNER_GITHUB_ID` to that account's numeric GitHub user ID, add `NTFY_TOPIC` and `NTFY_TOKEN`, set `NTFY_ENABLED=1`, and restart Raises. Startup fails if that owner ID does not resolve to the signed-in account. `NTFY_MIN_NOTICE_LEVEL` (`info`, `warning` or `error`, default `warning`) is the lowest informational-notice level pushed to ntfy; new and regressed errors always push, and quieter notices stay in `/v1/events` and configured webhooks. `WEBHOOK_ENCRYPTION_KEY` encrypts stored outbound webhook URLs, signing secrets, and destination authorization values.
