# Changelog

## 0.3.0

- Add `Raises.notify` for informational, warning, and error-level events.
- Preserve event and exception routes in the optional disk spool.
- Keep informational events separate from exception grouping and GitHub issues.

## 0.2.0

- Add opt-in, bounded disk spooling through `RAISES_SPOOL_DIR`.
- Retry transient delivery failures safely across Rails processes and restarts.
- Keep ingestion credentials out of spool files.

## 0.1.0

- Initial Rails 7.1+ `Rails.error` subscriber.
- Project-scoped ingestion authentication.
- Filtered request metadata and revision reporting.
