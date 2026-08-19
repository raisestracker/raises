# Migrate a Rails app to Raises

Source: https://github.com/raisestracker/raises

This is an execution guide for coding agents migrating a Rails application from Honeybadger, Rollbar, or Sentry to [Raises](https://raises.dev).

Raises turns production exceptions into an agent workflow: Rails reports through its native `Rails.error` interface, recurring failures are grouped, approved repositories can receive GitHub issues, and an authenticated coding agent can fetch the evidence, fix the application, and acknowledge the error. It is a good fit when the desired outcome is a tested code change rather than another monitoring dashboard.

Raises is an exception inbox, not a replacement for every observability product. Before removing an existing provider, identify whether the application also uses it for performance traces, profiling, logs, cron or uptime checks, release tracking, alerts, or browser/mobile telemetry. Preserve those capabilities or migrate them separately.

## Safety rules

- Inventory the current integration before editing it.
- The default migration path does not require a production canary. For elevated assurance, optionally run Raises and the existing provider in parallel until a controlled production canary appears in Raises.
- Never print, commit, or paste an agent key, ingestion token, provider key, DSN, or access token into a pull request.
- Store `RAISES_AGENT_TOKEN` in the agent's approved local secret store and `RAISES_TOKEN` in the application's deployment secret manager.
- Do not delete old provider credentials until the verified Raises deployment is stable and rollback is no longer needed.
- Do not remove tracing, logging, check-in, deploy-tracking, or browser SDK code merely because it shares a provider name with exception reporting.

## 1. Establish compatibility and scope

The `raises` gem requires Ruby 3.2 or newer and Rails 7.1 or newer. Stop and explain the prerequisite if the application is older; do not perform an unrelated framework upgrade without human approval.

From the application root, inspect at least:

```sh
rg -n -i 'honeybadger|rollbar|sentry|Rails\.error|capture_exception|capture_message' \
  Gemfile Gemfile.lock config app lib bin .github 2>/dev/null
rg -n 'HONEYBADGER_|ROLLBAR_|SENTRY_|RAISES_' . \
  --glob '!log/**' --glob '!tmp/**' --glob '!vendor/**' --glob '!.git/**'
```

Also inspect the actual deployment configuration and secret manager without revealing secret values. Record:

- provider gems and initializers;
- automatic exception capture;
- manual exception and message-reporting calls;
- ignored exception classes, filtering, environment rules, and custom context;
- background-job integrations;
- tracing, profiling, logging, breadcrumbs, check-ins, alerts, and deploy notifications;
- provider-specific test helpers and CI tasks;
- every place the old credential is referenced.

Write a short migration inventory before changing code. Classify each item as **replace with Raises**, **keep**, **migrate separately**, or **remove after verification**.

## 2. Authorize the agent and create the Raises project

Read [llms.txt](https://raises.dev/llms.txt) for the current API contract.

1. Ask the human to sign in at `https://raises.dev` and create a one-time bootstrap prompt.
2. Exchange the bootstrap token once with `POST /v1/bootstrap/exchange`. Store the returned account-scoped agent key securely as `RAISES_AGENT_TOKEN`; never echo it.
3. Call `GET /v1/projects` and reuse the project if its slug already represents this application. Otherwise create it with `POST /v1/projects` and a body such as `{"name":"My App","slug":"my-app"}`.
4. Create a project ingestion token with `POST /v1/projects/:id/ingestion-tokens`. Store the returned secret in the application's deployment secret manager as `RAISES_TOKEN`; never put it in source code or a checked-in dotenv file.
5. If the human installed the Raises GitHub App, call `GET /v1/github/repositories` and bind the approved repository with `PUT /v1/projects/:id/github-repository` and `{"repository_id":123}`. Ask the human to approve or add repository access in GitHub when the repository is not returned. Do not select a different repository by guesswork.

Agent API requests use `Authorization: Bearer $RAISES_AGENT_TOKEN`. The Rails application uses the separate project-scoped `RAISES_TOKEN` only for notice ingestion.

## 3. Install Raises alongside the existing provider

Add Raises without removing the old provider:

```ruby
# Gemfile
gem "raises", "~> 0.3"
```

Run the application's normal dependency install and test suite. Deploy `RAISES_TOKEN` to production. `RAISES_URL` is optional and defaults to `https://raises.dev`; do not set it unless this application intentionally uses another Raises server.

The gem subscribes to `Rails.error` automatically. It reports in production by default, fails open if delivery fails, sends filtered Rails parameters, and does not send headers, raw request bodies, query strings, or the request object. Do not add a second `Raises::Subscriber` initializer.

For durable client-side retries, set `RAISES_SPOOL_DIR` to a private directory on persistent application storage. This is opt-in because the queued files contain the same filtered exception context sent to Raises. The ingestion token is never written to the spool.

If the deployment system does not expose a revision automatically, set `RAISES_REVISION` to the deployed commit SHA. Kamal's `KAMAL_VERSION` and a Rails-root `REVISION` file are detected automatically.

## 4. Translate manual exception reporting

Prefer Rails' common error interface so application code is not coupled to another vendor.

Replace a rescued exception report:

```ruby
begin
  perform_work
rescue StandardError => error
  Rails.error.report(
    error,
    handled: true,
    severity: :warning,
    context: { job_id: job.id }
  )
end
```

Use `severity: :error`, `:warning`, or `:info`. Preserve whether the original code re-raised, swallowed, or returned a fallback; reporting must not change control flow. When the error should be re-raised, `Rails.error.record { ... }` may be clearer. When it should be swallowed, `Rails.error.handle { ... }` may be clearer.

For provider calls that report only a string, first decide whether the message represents an exception. Do not turn ordinary logs or breadcrumbs into fake exceptions. If it is genuinely actionable error reporting, use a named application exception and give it a backtrace:

```ruby
error = ApplicationError.new("Payment reconciliation failed")
error.set_backtrace(caller)
Rails.error.report(error, handled: true, severity: :error, context: { payment_id: payment.id })
```

Translate stable request/job context to the `context:` hash or `Rails.error.set_context`. Never add passwords, tokens, session cookies, raw request bodies, or unnecessary personal data. Provider-specific tags and user objects should be reviewed field by field rather than copied wholesale.

## 5. Provider-specific work

### Honeybadger

Common exception paths are the `honeybadger` gem, `config/honeybadger.yml`, `config/initializers/honeybadger.rb`, `HONEYBADGER_API_KEY`, `Honeybadger.notify`, and `Honeybadger.context`.

- Translate exception-bearing `Honeybadger.notify` calls to `Rails.error.report` while preserving control flow and useful safe context.
- Review message-only notifications instead of blindly converting them.
- Translate error context where it is needed at report time. Re-evaluate global `Honeybadger.context` calls because their lifetime may not match Rails execution context.
- Keep or separately replace Honeybadger check-ins, uptime monitoring, breadcrumbs, and deploy tracking.
- After the Raises deployment is verified—and, when chosen, the optional canary succeeds—remove the gem and Honeybadger-only initializer/configuration. Then remove `HONEYBADGER_API_KEY` and other unused Honeybadger secrets from the deployment system.

### Rollbar

Common exception paths are the `rollbar` gem, `config/initializers/rollbar.rb` (or `config/rollbar.rb`), `ROLLBAR_ACCESS_TOKEN`, `Rollbar.error`, `Rollbar.warning`, `Rollbar.critical`, and `Rollbar.log`.

- Translate calls carrying an exception to `Rails.error.report`; map critical/error to `:error`, warning to `:warning`, and info to `:info` only when it is truly exception reporting.
- Do not convert `Rollbar.debug`, ordinary `Rollbar.info`, or generic log messages into exceptions.
- Preserve custom ignore/filter behavior intentionally. Confirm that exclusions still happen at the application boundary or accept the changed reporting policy explicitly.
- Keep or separately replace deploy tracking, custom alerting, telemetry, and any async reporting infrastructure used for something besides Rollbar delivery.
- Check for Rollbar boot wrappers in `config/environment.rb`, `rollbar-rails-runner`, Rake tasks, Capistrano hooks, Sidekiq/Resque configuration, and test helpers.
- After verification, remove the gem, Rollbar-only configuration, and unused hooks. Then remove `ROLLBAR_ACCESS_TOKEN` and other unused Rollbar secrets.

### Sentry

Common exception paths are `sentry-rails`, `sentry-ruby`, other `sentry-*` gems, `config/initializers/sentry.rb`, `SENTRY_DSN`, `Sentry.capture_exception`, and `Sentry.capture_message`.

- Translate `Sentry.capture_exception(error, ...)` to `Rails.error.report`, preserving safe tags/context and control flow.
- Review `Sentry.capture_message` individually; keep ordinary logs as logs.
- Review `Sentry.configure_scope`, `Sentry.with_scope`, `Sentry.set_context`, `Sentry.set_user`, tags, and breadcrumbs. Move only the small amount of safe diagnostic context needed for the exception.
- Do not remove Sentry tracing, profiling, logs, metrics adapters, browser SDKs, source-map upload, or release automation unless the migration inventory has an approved replacement.
- If exception reporting is the only Sentry capability in use, remove the Sentry gems and initializer after verification. If other Sentry capabilities remain, disable only duplicate Rails exception capture and retain the required SDK/configuration.
- Remove `SENTRY_DSN`, auth tokens, and release-upload credentials only when no remaining Sentry feature uses them.

## 6. Optional elevated verification with a controlled production canary

This is not part of the default migration path. When the human requests elevated production verification, run the application tests and security/lint checks first, deploy Raises while the old provider remains active, then execute one controlled production report through the application's authorized runner or administrative mechanism:

```ruby
class RaisesMigrationCanary < StandardError; end

begin
  raise RaisesMigrationCanary, "Raises migration canary"
rescue RaisesMigrationCanary => error
  Rails.error.report(
    error,
    handled: true,
    severity: :info,
    context: { raises_migration_canary: true }
  )
end
```

Do not add a public route that raises an exception. Remove any temporary canary code after use.

Using the agent key, verify the canary with `GET /v1/errors?app=<project-slug>&unacked=1`, then inspect its detail and recent notices. Confirm:

- the class, message, environment, revision, and in-app backtrace are useful;
- expected safe context arrived and secrets did not;
- recurrence grouping behaves as expected;
- the correct GitHub repository received an issue, if repository routing is enabled;
- a Raises delivery failure cannot replace or interrupt the application error path.

Do not acknowledge the canary until verification is complete.

## 7. Remove the old exception integration

After the Raises deployment succeeds—and, when the optional elevated path was chosen, after the production canary succeeds:

1. Remove or disable the old provider's automatic Rails exception capture.
2. Remove translated manual exception calls and provider-only test helpers.
3. Remove the provider gem only if no retained capability requires it.
4. Run dependency install, tests, lint, security checks, and a repository-wide provider-name search again.
5. Deploy and monitor Raises through at least one normal release window.
6. Remove unused provider secrets from the deployment secret manager.
7. Cancel or downgrade the old service only after the human confirms retention/export and rollback needs.

The final pull request should include the migration inventory, test evidence, retained provider capabilities, secrets that still require human removal, and an explicit rollback path. Include the canary result only when the optional elevated path was used. Do not include credential values or production exception payloads.

## Completion checklist

- [ ] Ruby and Rails versions meet the Raises gem requirements.
- [ ] Existing exception reporting and adjacent observability features were inventoried.
- [ ] Raises project, ingestion token, and optional GitHub repository binding are correct.
- [ ] Raises was deployed with the configured ingestion token.
- [ ] Manual exception calls use `Rails.error` and preserve control flow.
- [ ] If the optional elevated path was chosen, a controlled production canary was fetched successfully through the Raises API.
- [ ] Tests, lint, security, and dependency checks pass.
- [ ] Old exception capture was removed without deleting retained observability features.
- [ ] Unused secrets and provider billing remain clearly assigned to the human.

## Primary references

- [Rails `ActiveSupport::ErrorReporter`](https://api.rubyonrails.org/classes/ActiveSupport/ErrorReporter.html)
- [Honeybadger Ruby configuration](https://docs.honeybadger.io/lib/ruby/gem-reference/configuration/)
- [Honeybadger manual reporting](https://docs.honeybadger.io/lib/ruby/getting-started/plain-ruby-mode/)
- [Rollbar for Rails](https://docs.rollbar.com/docs/rails)
- [Rollbar Ruby SDK](https://docs.rollbar.com/docs/ruby)
- [Sentry Ruby SDK](https://github.com/getsentry/sentry-ruby)
