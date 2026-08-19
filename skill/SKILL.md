---
name: raises
description: Set up Raises or investigate scoped production exceptions and open a draft fix PR.
---

# Raises

Source: https://github.com/raisestracker/raises

Read `https://raises.dev/llms.txt` before setup. Use `Authorization: Bearer $RAISES_AGENT_TOKEN` for agent endpoints. Keep the token in the user's approved secret manager; never commit or echo it.

## Setup

1. Ask the human to sign in at `https://raises.dev` and create a one-time bootstrap prompt.
2. Exchange the bootstrap credential, store the returned agent key securely, and create a project through the API.
3. Create a project ingestion token, add `gem "raises"`, and configure `RAISES_TOKEN` in the application secret manager.
4. If the human installed the optional GitHub App, bind one of the repositories returned by `/v1/github/repositories`.
5. Only when the human requests elevated production verification, trigger a controlled exception and verify it through the scoped errors API.

Use `Raises.notify("message", level: :info, context: {}, source: nil)` for operational information that must not create an error group or GitHub issue. Query retained notices through `/v1/events`.

Agents can manage signed account-scoped integrations through `/v1/webhook-endpoints`. Store each one-time signing secret securely, send a test delivery, inspect `/v1/webhook-deliveries`, and retry only dead deliveries.

## Investigate

1. `GET /v1/errors?app=<project-slug>&unacked=1`
2. `GET /v1/errors/:id`
3. `GET /v1/errors/:id/notices?limit=5`
4. Open the first in-app file from `location` and reproduce the error.
5. Implement and test the smallest safe fix.
6. Open a draft PR referencing `github_issue_url` when present.
7. `POST /v1/errors/:id/ack` only after the draft PR exists.

Do not merge, close issues, expose credentials, or make production changes without the user's authorization.
