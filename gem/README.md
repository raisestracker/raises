# raises

The gem is MIT-licensed; see [LICENSE.txt](LICENSE.txt). The Raises server and embedded web UI are separately AGPL-3.0-only.

The Rails client for [Raises](https://raises.dev), error reporting operated by coding agents.

```ruby
gem "raises"
```

Set `RAISES_TOKEN` to the ingestion credential created for the project. Production errors are reported to `https://raises.dev` automatically through `Rails.error`. Set `RAISES_REPORT=1` to exercise the integration outside production.

Send a one-off operational notice without raising an exception:

```ruby
Raises.notify(
  "Import finished",
  level: :info,
  source: "nightly-import",
  context: { imported: 412, skipped: 3 }
)
```

Valid levels are `:info`, `:warning`, and `:error`. Notices are retained and delivered to configured outbound integrations, but they do not create error groups, acknowledgements, or GitHub issues.

Optional settings are `RAISES_URL`, `RAISES_ENV`, `RAISES_REVISION`, `RAISES_OPEN_TIMEOUT`, and `RAISES_READ_TIMEOUT`.

Set `RAISES_SPOOL_DIR` to add restart-safe, disk-backed delivery retries. The directory must be on persistent storage if payloads should survive a deploy. Raises stores event or exception JSON there with private permissions, never stores the ingestion token, retries network errors, HTTP 408/429, and 5xx responses, and bounds the spool at 1,000 payloads or 100 MB. Leave it unset to preserve synchronous best-effort delivery.

The reporter fails open: network or serialization failures never replace the application exception. Rails filtered parameters are included; headers, raw bodies, query strings, and the request object are not.
