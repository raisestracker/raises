# Deployment

Production deploys happen automatically when a PR merges to `master`. The `deploy` job in `.github/workflows/ci.yml` needs the `PR tests` (`go`), `docker`, and `acceptance` jobs to pass on that same commit, so a red CI run never deploys. (The `go` job runs `bin/check` with `SKIP_ACCEPTANCE=1` because the self-hosted runner has no container runtime; the `acceptance` job covers that suite.) It then runs `bin/release` (`kamal deploy`, then `/healthz` and `/readyz` smoke tests), checks that `kamal app version` reports the merged SHA, and runs `bin/release-tag` to tag the release and cut GitHub release notes from the merged PRs. Deploys run one at a time (`concurrency: production`, no cancel). If a deploy fails, Kamal keeps serving the previous release.

## GitHub `production` environment

The environment is restricted to the `master` branch (deployment branch policy).

- Variable `KAMAL_DEPLOY_ENV`: the full contents of production's `.kamal/deploy.env`. Source of truth is the `raises-deploy-env` document in the rails 1Password vault (`op document get raises-deploy-env --vault rails`). The job writes it to `.kamal/deploy.env`, so automatic and manual deploys use the same config. Keep it in sync when production settings change.
- Secrets: `DEPLOY_SSH_KEY` (a deploy-only ed25519 key, comment `raises-ci-deploy`), `DEPLOY_KNOWN_HOSTS`, `TS_OAUTH_CLIENT_ID`, `TS_OAUTH_CLIENT_SECRET` (Tailscale OAuth client, see below), `RAISES_GITHUB_APP_ID`, `RAISES_GITHUB_APP_CLIENT_ID`, `RAISES_GITHUB_APP_CLIENT_SECRET`, `RAISES_GITHUB_APP_PRIVATE_KEY`, `RAISES_GITHUB_APP_WEBHOOK_SECRET`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `NTFY_TOKEN`, `NTFY_TOPIC`, `WEBHOOK_ENCRYPTION_KEY`. (GitHub reserves the `GITHUB_` secret prefix, hence `RAISES_`.) The app secrets live in the `raises` item in the rails vault.
- Tailscale: `KAMAL_HOST` resolves only on the tailnet (MagicDNS), so the deploy job joins it with `tailscale/github-action` before deploying. One human step: create a Tailscale OAuth client allowed to tag devices `tag:ci` ([admin console](https://login.tailscale.com/admin/oauth-clients)) and store its id/secret as `TS_OAUTH_CLIENT_ID` / `TS_OAUTH_CLIENT_SECRET` in the `production` environment.
- Authorizing the deploy key (one human step per key): on a machine with production's `deploy.env`, run

```sh
set -a; . .kamal/deploy.env; set +a
echo 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBz5i7QwsPh5DSkgQxBeJ/oC9lGPCXigWR++FpGRgH0FCa raises-ci-deploy' | ssh "$KAMAL_SSH_USER@$KAMAL_HOST" 'mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys'
```

To rotate the key, generate a new pair, replace the `DEPLOY_SSH_KEY` secret, and authorize the new public key the same way.

## Releases

Every deploy tags the deployed commit (`vX.Y.Z` on `master`) and cuts a GitHub release whose notes list the merged PRs since the previous tag. Version rule: minor when a commit subject in range starts with `feat:`/`feature:` (case-insensitive, optional scope), otherwise patch; `bin/release-tag [--minor|--patch]` overrides it. The deploy job runs it only after the live-version check passes, so the tag always matches production. Reruns are no-ops when nothing changed. `bin/deploy` runs it too, so manual fallback deploys also cut releases.

## Rolling back

Preferred: revert the bad PR on `master`; the merge deploys the revert. For an immediate rollback, from a machine with `.kamal/deploy.env`:

```sh
kamal app version          # what is live
kamal rollback <previous-version>
```

## Manual fallback

`bin/deploy` runs the same checks as the CI job (`bin/check`), pushes `master`, then `bin/release` and `bin/release-tag`. Needs `gh` for the release notes. Use it only when Actions is unavailable.
