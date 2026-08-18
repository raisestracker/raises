# Raises API

Source: [github.com/raisestracker/raises](https://github.com/raisestracker/raises).

All request and response bodies are JSON. Agent endpoints use `Authorization: Bearer $RAISES_AGENT_TOKEN`; ingestion uses the project credential instead.

## Bootstrap

`POST /v1/bootstrap/exchange`

```json
{"token":"boot_…","name":"My coding agent"}
```

Returns an account-scoped agent key once. Bootstrap credentials expire after ten minutes and cannot be reused.

## Projects

- `GET /v1/projects?archived=1`
- `POST /v1/projects` with `{"name":"Widget","slug":"widget"}`
- `GET /v1/projects/:id`
- `PATCH /v1/projects/:id` with `{"name":"New display name"}`
- `POST /v1/projects/:id/archive`
- `POST /v1/projects/:id/restore`
- `POST /v1/projects/:id/ingestion-tokens`
- `DELETE /v1/projects/:id/ingestion-tokens/:token_id`

Project deletion is intentionally unavailable. Archiving is reversible and disables ingestion.

## GitHub

- `GET /v1/github/repositories`
- `PUT /v1/projects/:id/github-repository` with `{"repository_id":123}`
- `DELETE /v1/projects/:id/github-repository`

The repository must belong to a GitHub App installation connected by the signed-in account owner.

## Exceptions

- `POST /v1/notices` using a project ingestion token
- `GET /v1/errors?app=widget&unacked=1`
- `GET /v1/errors/:id`
- `GET /v1/errors/:id/notices?limit=5`
- `POST /v1/errors/:id/ack`

An agent key can access only projects owned by its GitHub account. The API returns `429` with `Retry-After` when a rate or account limit is reached.

## Informational events

`POST /v1/events` uses a project ingestion token:

```json
{
  "env": "production",
  "revision": "abc123",
  "level": "info",
  "message": "Import finished",
  "source": "nightly-import",
  "context": {"imported": 412, "skipped": 3}
}
```

`message` is required and limited to 2,000 characters. `level` defaults to `info` and accepts `info`, `warning`, or `error`. `source` is optional and limited to 120 characters. `env` is limited to 100 characters, `revision` to 200, and context to 64 KB of JSON.

Agent endpoints:

- `GET /v1/events?project=widget&level=info&since=2026-08-16T00:00:00Z&limit=50`
- `GET /v1/events/:id`

Events are account-scoped and retained for 30 days, with a maximum of the newest 10,000 events per account. They do not participate in exception grouping, acknowledgement, or GitHub issue creation.

## Outbound webhooks

An account can have up to three active webhook endpoints. All routes require an agent token.

- `GET /v1/webhook-endpoints`
- `POST /v1/webhook-endpoints` with `{"url":"https://example.com/raises","events":["notice.created"]}`
- `PATCH /v1/webhook-endpoints/:id` with the complete replacement `url`, `events`, and `active` values
- `DELETE /v1/webhook-endpoints/:id`
- `POST /v1/webhook-endpoints/:id/rotate-secret`
- `POST /v1/webhook-endpoints/:id/test`
- `GET /v1/webhook-deliveries?state=dead&limit=50`
- `POST /v1/webhook-deliveries/:id/retry`

Supported subscriptions are `notice.created`, `error.created`, `error.regressed`, `github_issue.opened`, and `github_issue.reopened`. `webhook.test` is emitted only by the test route. Omitting `events` subscribes to all non-test events.

Create and rotation responses return `signing_secret` once. Raises encrypts endpoint URLs and signing secrets at rest. Store the secret immediately; list responses never include it.

Each request is an HTTPS `POST` with this envelope:

```json
{
  "id": "obe_…",
  "type": "notice.created",
  "created_at": "2026-08-16T12:00:00Z",
  "project": {"id": "prj_…", "name": "Widget"},
  "data": {"notice": {"message": "Import finished"}}
}
```

Headers are `X-Raises-Delivery`, `X-Raises-Event`, `X-Raises-Timestamp`, and `X-Raises-Signature`. Verify `X-Raises-Signature` as lowercase hex HMAC-SHA256 using the exact request body and this signed value:

```text
<X-Raises-Timestamp>.<raw request body>
```

The signature header is `v1=<hex digest>`. Reject stale timestamps and compare signatures in constant time. Raises accepts HTTPS endpoints on port 443 only, blocks private and local destination addresses, does not follow redirects, retries transient network responses with backoff, and moves a delivery to dead-letter state after 20 attempts. Completed and dead delivery history is retained for 30 days.
