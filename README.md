# Raises

[Raises](https://raises.dev) is error reporting for coding agents. Rails exceptions become structured evidence an agent can investigate, fix, and acknowledge. The hosted service supports optional GitHub issue creation and reopening.

Self-hosters can use the small local Docker Compose setup in the [self-hosting guide](docs/self-hosting.md).

## Configuration

| Variable | Purpose |
|---|---|
| `DATABASE_PATH` | SQLite database path |
| `BASE_URL` | Public server URL |
| `GITHUB_APP_*` | Optional GitHub App configuration; set all required values together |
| `WEBHOOK_ENCRYPTION_KEY` | Optional at-rest encryption for outbound webhook secrets |
| `REPORT_*`, `AWS_REGION` | Optional SES reports and operational alerts |
| `NTFY_*` | Optional ntfy notifications |
| `INITIAL_OWNER_GITHUB_ID` | Optional legacy project owner migration |

Health endpoints are `GET /healthz` and `GET /readyz`. Agent and ingestion interfaces are documented in [docs/api.md](docs/api.md). Agent-readable setup is at `/llms.txt`, `/skill.md`, and `/migration.md`.

## Rails gem

```ruby
gem "raises", "~> 0.3"
```

Configure `RAISES_TOKEN` with a project ingestion token and use `RAISES_URL` when the Rails app should report to a self-hosted server. See [raisestracker/raises-rails](https://github.com/raisestracker/raises-rails).

## Licensing

The Go server and embedded web UI are licensed under [AGPL-3.0-only](LICENSE).

## Contributing

Please open issues and proposals at [raisestracker/raises](https://github.com/raisestracker/raises). See [CONTRIBUTING.md](CONTRIBUTING.md).
