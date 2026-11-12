# Acceptance suite

End-to-end tests that run the real Raises server and a real SQLite database,
then drive them the way people and agents do:

- **Browser flows** in Chromium and WebKit (Safari's engine): GitHub sign-in,
  settings, creating a normal agent key and an account key, the bootstrap
  prompt (it appears in place, copies, and the page doesn't jump to the top),
  and revoking a key (`tests/ui.spec.ts`).
- **The agent and ingestion API**: bootstrap exchange, account-key minting
  (`/v1/agent-keys` mint, list, revoke), projects, ingestion tokens, ingesting
  notices, listing, acknowledging and suppressing errors, account isolation,
  and webhook endpoints (`tests/api.spec.ts`).
- **Cross-checks**: a key created in the browser works on the API, and a key
  revoked in the browser is rejected by it.
- **Error paths**: the key form shows the server's own error messages, never a
  browser JSON-parse exception such as Safari's "The string did not match the
  expected pattern."

## How it works

`cmd/raises-acceptance` builds the same HTTP server and store as
`cmd/raises`, on a fresh database seeded with two accounts. It replaces only
the parts that would reach the network:

- GitHub OAuth completes locally and signs in the seeded owner, so
  `/auth/github` and its callback still run for real.
- Outbound webhook sends are recorded in memory after the real queue and
  worker. Tests read them from `GET /__acceptance/deliveries`.

The harness also serves `GET /__acceptance/sign-in`, which returns a session
cookie and CSRF token so API tests can use the settings endpoints without a
browser. It refuses to listen on anything but loopback. Never run it as a
real server.

Playwright starts the harness (`webServer` in `playwright.config.ts`) and runs
the tests serially against one database. Nothing leaves the machine.

## Run it locally

```sh
bin/acceptance
```

The script installs the pinned Playwright package, builds the harness, and
runs the tests in the official Playwright container
(`mcr.microsoft.com/playwright`), using docker or podman. The container has
every library WebKit needs, and it's the same image CI uses. The full run
takes about a minute and a half.

Arguments go to `playwright test`:

```sh
bin/acceptance --project=webkit           # one browser
bin/acceptance -g "bootstrap"             # tests matching a title
bin/acceptance --project=api              # API only
```

To use browsers installed on your machine instead of the container:

```sh
(cd acceptance && npx playwright install chromium webkit)
ACCEPTANCE_NATIVE=1 bin/acceptance
```

When a test fails, screenshots and traces are written to
`acceptance/test-results/`. Open a trace with
`npx playwright show-trace acceptance/test-results/<test>/trace.zip`.

## Where it runs

- CI runs the suite on every pull request and on pushes to master
  (`acceptance` job in `.github/workflows/ci.yml`). A failed run uploads the
  report as the `acceptance-report` artifact.
- `bin/deploy` runs it before deploying and stops if it fails.

## Upgrading Playwright

Change the `@playwright/test` version in `package.json` (and the lockfile),
the container tag in `.github/workflows/ci.yml`, and the version check in the
same job. `bin/acceptance` reads the image tag from `package.json`.
