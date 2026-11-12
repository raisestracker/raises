# Deployment

Production deploys happen automatically when a PR merges to `master`. The `deploy` job in `.github/workflows/ci.yml` needs the `go` and `docker` jobs to pass on that same commit, so a red CI run never deploys. It then runs `bin/release` (`kamal deploy`, then `/healthz` and `/readyz` smoke tests) and checks that `kamal app version` reports the merged SHA. Deploys run one at a time (`concurrency: production`, no cancel). If a deploy fails, Kamal keeps serving the previous release.

## GitHub `production` environment

Restrict the environment to the `master` branch. Variable: `KAMAL_DEPLOY_ENV`, the full contents of production's `.kamal/deploy.env` (see `.kamal/deploy.env.example`; include `DISABLE_LEGACY_TOKENS`, `GITHUB_APP_SLUG` and the report/alert/NTFY settings if production sets them). The job writes it to `.kamal/deploy.env`, so automatic and manual deploys use the same config. Keep it in sync when production settings change. Secrets: `DEPLOY_SSH_KEY` (a key limited to deploys), `DEPLOY_KNOWN_HOSTS`, `RAISES_GITHUB_APP_ID`, `RAISES_GITHUB_APP_CLIENT_ID`, `RAISES_GITHUB_APP_CLIENT_SECRET`, `RAISES_GITHUB_APP_PRIVATE_KEY`, `RAISES_GITHUB_APP_WEBHOOK_SECRET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `NTFY_TOKEN`, `NTFY_TOPIC`, `WEBHOOK_ENCRYPTION_KEY`. (GitHub reserves the `GITHUB_` secret prefix, hence `RAISES_`.)

## Rolling back

Preferred: revert the bad PR on `master`; the merge deploys the revert. For an immediate rollback, from a machine with `.kamal/deploy.env`:

```sh
kamal app version          # what is live
kamal rollback <previous-version>
```

## Manual fallback

`bin/deploy` runs the same checks as the CI `go` job (`bin/check`), pushes `master`, then `bin/release`. Use it only when Actions is unavailable.
