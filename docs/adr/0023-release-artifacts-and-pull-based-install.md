# ADR-0023: Release artifacts and a pull-based install script

- Status: accepted
- Date: 2026-10-19

## Context

`#132` asked how the platform is built, released and put on servers. The repository had no CI, no releases, and every binary reported version `dev`, although ADR-0014's handshake and `outdated` flag depend on the Agent version. `deploy/compose.yaml` serves e2e: it carries development credentials and `127.0.0.1` addresses that only work when Nodes share the Control Plane host. ADR-0014 already fixes the operational shape: upgrades are operator actions, Control Plane first and Agents after, migrations are forward-only, and Agents are neither self-updating nor signature-verified (GitHub's runner checks a SHA-256; Coder serves the binary from the server).

## Decision

**Artifacts.** A tag `vX.Y.Z` produces one GitHub Release and one image:

- `ghcr.io/zibyn/stars-sandbox-server:X.Y.Z` (and `:latest`), public, linux amd64 and arm64, with the Web console embedded. Every push to `main` also publishes `:main`.
- The Node package: `stars-sandbox-agent` and `stars-sandbox` in one archive per linux architecture, because the Agent finds the CLI beside its own executable (`runtime.md`).
- The CLI alone for darwin amd64 and arm64.
- `SHA256SUMS` over every archive.

The version is the tag, stamped into all three binaries at build time; the Agent reports it in the handshake.

**CI gate.** Every pull request and push to `main` runs vet, unit tests, the Web console checks, the embedded build, the Python SDK tests and the Compose e2e suite. Scenarios that need a GPU or a remote host skip themselves.

**Pull-based install.** Servers fetch `deploy/install.sh` from the release tag on GitHub and run it; GitHub holds no server credentials and never connects to a server. The version is a required argument, never `latest`. The script checks for Docker, Podman or Compose and fails if they are missing; it does not install them.

- `install.sh server` sets up a Control Plane host: the Control Plane, PostgreSQL, the object store and the registry, from a release Compose file without development credentials or e2e services. On first run it generates the `node` surface TLS certificate and every secret, and never regenerates them. Each upgrade dumps PostgreSQL first; a downgrade is refused, because migrations are forward-only and rollback is restoring a backup (ADR-0014). PostgreSQL and the object store may point at external services instead.
- `install.sh node` installs the Node package, verifies it against `SHA256SUMS`, enrolls once with an Enrollment Packet, and runs the Agent as a systemd service whose stop signal reaches the Agent only, so Exec Supervisors keep running across a restart. An older Agent is allowed: the Control Plane accepts an older minor and flags the Node `outdated`.

Upgrading is re-running the same script with a newer version, Control Plane first.

## Consequences

- Deploying needs a shell on each server; there is no fleet-wide push.
- A forgotten Node keeps running its old Agent until someone re-runs the script, visible as `outdated`.
- The SHA-256 check guards against a corrupted download, not a compromised GitHub account.
- The release Compose file and the e2e Compose file are maintained separately.

## Rejected alternatives

- CI deploying over SSH: GitHub would hold credentials for every server, and ADR-0014 already makes upgrades an ordered operator action.
- A `latest` install path: one unattended re-run would upgrade a Control Plane past a forward-only migration.
- Signing the Node package: no vendor does it for its agent (ADR-0014), and the checksum covers transport.
- Installing Docker or Podman from the script: distribution package managers differ, and the host's container runtime belongs to its operator.
- Reusing `deploy/compose.yaml` for production: its defaults are development credentials and same-host addresses.
