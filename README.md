# Raises

[Raises](https://raises.dev) is error reporting for coding agents. Rails exceptions become structured evidence an agent can investigate, fix, and acknowledge. The hosted service supports optional GitHub issue creation and reopening.

## Get started

Sign in at [raises.dev](https://raises.dev), create a one-time bootstrap prompt, and follow the agent setup guide at [`/llms.txt`](https://raises.dev/llms.txt). The full agent workflow is documented at [`/skill.md`](https://raises.dev/skill.md).

Migrating from Honeybadger, Rollbar, or Sentry? Read [`/migration.md`](https://raises.dev/migration.md).

## Rails gem

```ruby
gem "raises", "~> 0.3"
```

Set `RAISES_TOKEN` to a project ingestion token. Use `RAISES_URL` when reporting to a self-hosted server. See [raisestracker/raises-rails](https://github.com/raisestracker/raises-rails).

## Self-hosting

See the [self-hosting guide](docs/self-hosting.md) for Docker Compose, GitHub App setup, backups, and optional integrations.

## API

See the [API reference](docs/api.md) for agent and ingestion interfaces.

## License

The server is licensed under [AGPL-3.0-only](LICENSE).

## Contributing

Issues and proposals are welcome at [raisestracker/raises](https://github.com/raisestracker/raises). See [CONTRIBUTING.md](CONTRIBUTING.md).
