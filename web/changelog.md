# What's new in Raises

Dated notes on what shipped, newest first.

Updated 2026-11-23.

## Week of 2026-11-16

- The homepage now links the [Agent skill](https://raises.dev/skill.md), so agents and search can find it.
- Valid API requests are no longer rejected while the service absorbs invalid-token traffic. Rate limiting checks the per-IP budget before authentication and applies the global budget on the failure path only.
