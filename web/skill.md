---
name: raises
description: Set up Raises or investigate scoped production exceptions and open a draft fix PR.
---

# Raises

Read https://raises.dev/llms.txt first.

For migrations from Honeybadger, Rollbar, or Sentry, read https://raises.dev/migration.md. Its parallel-verification canary is an optional elevated-assurance path; use it only when the human requests production verification.

Use `Authorization: Bearer $RAISES_AGENT_TOKEN` for agent endpoints. Keep the token in the user's approved secret manager; never commit or echo it.

## Investigate

1. `GET /v1/errors?app=<project-slug>&unacked=1`
2. `GET /v1/errors/:id`
3. `GET /v1/errors/:id/notices?limit=5`
4. Open the first in-app file from `location` and reproduce the error.
5. Implement and test the smallest safe fix.
6. Open a draft PR referencing `github_issue_url` when present.
7. `POST /v1/errors/:id/ack` only after the draft PR exists.

Known false positives can be suppressed with `POST /v1/errors/:id/suppress`. Suppression is store-only for that exact group: Raises keeps notices and increments counts, but future recurrences do not clear acknowledgement, do not enqueue webhooks or ntfy, and do not open or reopen GitHub issues. The group disappears from `unacked=1` listings without deleting evidence. Restore normal behavior for future occurrences with `POST /v1/errors/:id/unsuppress`; unsuppress never retroactively delivers missed notifications or changes an existing GitHub issue.

Do not merge, close issues, expose credentials, or make production changes without the user's authorization.

## Informational notices

Use `Raises.notify("message", level: :info, context: {}, source: nil)` when the application should report an operational event without creating an exception or GitHub issue. Query these events with `GET /v1/events?project=<project-slug>&level=info` and fetch one with `GET /v1/events/:id`.

Manage account-scoped outbound destinations with `/v1/webhook-endpoints`. A create or secret-rotation response shows its signing secret once. Store that value in the user's approved secret manager and never print or commit it. Use the test route before relying on a destination, inspect `/v1/webhook-deliveries`, and retry only dead deliveries. The full signing contract is at https://github.com/raisestracker/raises/blob/master/docs/api.md.
