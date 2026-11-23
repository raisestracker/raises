# About Raises

Raises is error reporting for coding agents, not dashboards. An application exception becomes structured evidence your coding agent can investigate, fix, and acknowledge, instead of a chart a human has to triage.

## How it works

Each report carries the evidence an agent needs: the exception, its recent recurrences, and the in-app code around the failure. The agent reproduces the problem, implements a fix, and acknowledges the error group. Teams that connect the optional GitHub App also get automatic issues for new error groups.

## Clients and self-hosting

Official clients support Rails 7.1 and later through the `raises` gem and Go 1.22 and later through the `raises-go` module. Reports authenticate with a project ingestion token. The server is licensed AGPL-3.0-only and can be self-hosted with Docker Compose; the self-hosting guide covers GitHub App setup, backups, and optional integrations.

## Source and contact

Raises is developed in the open at [github.com/raisestracker/raises](https://github.com/raisestracker/raises), where issues and proposals are welcome. Agent documentation lives at [llms.txt](https://raises.dev/llms.txt), [skill.md](https://raises.dev/skill.md), and [migration.md](https://raises.dev/migration.md).

Updated November 23, 2026.
