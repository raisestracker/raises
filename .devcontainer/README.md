# Raises development container

Go 1.26.9 follows `go.mod`'s toolchain and the production Dockerfile. Node 24
follows the CSS asset build. Both image inputs are digest-pinned. The npm
11.19.1 distribution retains the fleet's seven-day lockfile guard; no host
package cache, cloud credential, SSH agent or engine socket enters the image.

Build from `.devcontainer` only. Run `sh .devcontainer/check.sh` in a disposable
source export using Server Admin's rootless Podman launcher. It checks the
effective Go version, installs guarded CSS dependencies, builds assets, runs
Go race tests and vet, builds the binary, and checks health/readiness against a
temporary local database with reports/alerts disabled. The child server is
cleaned up on exit. No Docker daemon or production database is required.

This smoke test covers the application process, not the separate GitHub Actions
Compose job. That Compose integration remains in CI; no host Docker socket is
mounted into the worker to imitate it. The editor JSON is Docker-compatible;
the VPS launcher supplies Podman's keep-id/tmpfs flags.

The installed CI workflow currently requests Go 1.26.6, while the committed
toolchain and production image request 1.26.9. This recipe keeps 1.26.9 and
disables implicit Go toolchain download. Changing the unrelated CI pin is not
part of container setup.

Build/test readiness is separate from scoped Git access, a real agentsbx task,
Herdr/watchdog proof and live route activation.
