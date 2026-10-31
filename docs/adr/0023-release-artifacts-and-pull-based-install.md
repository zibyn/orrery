# ADR-0023: Release artifacts and a pull-based install script

- Status: accepted; amended in place by #135, #136 and #179 (CI gate runs once, on the pull request); deployment configuration layout amended by ADR-0024
- Date: 2026-10-19

## Context

`#132` asked how the platform is built, released and put on servers. The repository had no CI, no releases, and every binary reported version `dev`, although ADR-0014's handshake and `outdated` flag depend on the Agent version. `deploy/compose.yaml` serves e2e: it carries development credentials and `127.0.0.1` addresses that only work when Nodes share the Control Plane host. ADR-0014 already fixes the operational shape: upgrades are operator actions, Control Plane first and Agents after, migrations are forward-only, and Agents are neither self-updating nor signature-verified (GitHub's runner checks a SHA-256; Coder serves the binary from the server).

## Decision

**Artifacts.** A tag `vX.Y.Z` produces one GitHub Release and one image:

- `ghcr.io/zibyn/orrery-server:X.Y.Z` (and `:latest`), public, linux amd64 and arm64, with the Web console embedded. Every push to `main` also publishes `:main`.
- The Node package: `orrery-agent` and `orrery` in one archive per linux architecture, because the Agent finds the CLI beside its own executable (`runtime.md`).
- The CLI alone for darwin amd64 and arm64.
- `SHA256SUMS` over every archive.

The version is the tag, stamped into all three binaries at build time; the Agent reports it in the handshake.

**CI gate.** Every pull request runs vet, unit tests, the Web console checks, the embedded build, the Python SDK tests and the Compose e2e suite. Scenarios that need a GPU or a remote host skip themselves. A push to `main` or a `v*` tag does not run them again, since the merged pull request already passed them; it only publishes. A nightly run on `main`, which can also be started by hand, runs the full gate, and it covers changes pushed to `main` without a pull request.

**Pull-based install.** Servers fetch `deploy/install.sh` from GitHub and run it; GitHub holds no server credentials and never connects to a server. The version defaults to the newest published release: the script reads the tag from `releases/latest` and installs that tag's Compose file, configuration and binaries, never a branch. A bootstrap may fetch the script from `main`; the auto-updater fetches the resolved tag's script. An explicit `--version vX.Y.Z` pins an older or newer release. The script checks for Docker, Podman or Compose and fails if they are missing; it does not install them.

- `install.sh server` sets up a Control Plane host: the Control Plane, PostgreSQL, the object store, the registry and an nginx front, from a release Compose file without development credentials or e2e services. On first run it generates the deployment's TLS certificate and every secret, and never regenerates them. Each upgrade dumps PostgreSQL first; a downgrade is refused, because migrations are forward-only and rollback is restoring a backup (ADR-0014). PostgreSQL and the object store may point at external services instead.
- `install.sh node` installs the Node package, verifies it against `SHA256SUMS`, enrolls once with an Enrollment Packet, and runs the Agent as a systemd service whose stop signal reaches the Agent only, so Exec Supervisors keep running across a restart. An older Agent is allowed: the Control Plane accepts an older minor and flags the Node `outdated`.

**One public entry, HTTPS only.** The release stack runs nginx as the only public listener. It terminates TLS and routes one `addr:port` by the paths the protocols fix: `/v2/` to the registry, `/orrery.node.v1.Node/` to the `node` surface, everything else to `api`. The object store gets its own hostname on the same port, because a presigned URL signs the full path and SigV4 signs the `Host`, so it can be neither a subpath nor rewritten. `install.sh server` takes the public host and port (`https://HOST[:PORT]`, for example `:9443`) and derives `access.url` in the deployment configuration file (ADR-0024), the registry address and the `node` address from it; the object-store hostname defaults to `s3.HOST` and can be overridden.

The front has two certificate modes:

- `self-signed` (default): the script generates a CA and one certificate whose SANs cover the public host and the object-store host. Nodes trust the CA for the Agent and for both Docker and Podman trust directories; the Control Plane and the registry trust it for their own outbound calls.
- `cert`: the deployment supplies the certificate and key, and a CA file when it is private. The same certificate serves the public host and the object store.

An external reverse proxy may sit in front. It forwards `https://HOST:PORT` to the same port on the Control Plane host; when it shares the host, `--public-bind 127.0.0.1` leaves the public address to it. The `node` surface is reached through this front rather than by a direct connection, so Agents only need the public name; the process still listens on its own `node` address inside the host (ADR-0014). The registry is plain HTTP on loopback behind the front; dockerd and Podman only accept plain HTTP on loopback, and the front is what makes the registry HTTPS to Nodes.

Upgrading is re-running the same script with a newer version, Control Plane first.

**Automatic updates are opt-in.** `--auto-update` installs an updater that resolves the newest release and re-runs that release's install script: a separate Docker Compose project on the Control Plane and a systemd service on a Node, whose deployment is already systemd. `--no-auto-update` removes it. The updater is off by default. It goes through the same PostgreSQL backup, downgrade refusal and health check as a manual run, refuses to cross a major version, and shares a lock with manual runs. It never rolls back: a release that starts but misbehaves is handled by restoring the backup. A Node updater still has to follow the Control Plane, so it is only for deployments that accept a short refusal window or schedule it after the Control Plane's.

## Consequences

- Deploying needs a shell on each server; there is no fleet-wide push.
- A forgotten Node keeps running its old Agent until someone re-runs the script, visible as `outdated`.
- The SHA-256 check guards against a corrupted download, not a compromised GitHub account.
- The release Compose file and the e2e Compose file are maintained separately.
- Port forwarding over HTTPS needs a wildcard SAN or a wildcard certificate; the generated one only covers the public and object-store hosts.
- An external proxy must forward the same port number, because the registry and the object store sign URLs with the public `HOST:PORT`.
- Auto-update trades the operator's gate for freshness: a compromised GitHub account becomes code execution on every host that enabled it, and SHA-256 only detects a corrupted download.
- An auto-updated release can apply a forward-only migration unattended. The updater takes the PostgreSQL backup first, but rollback is still a manual restore.

## Rejected alternatives

- CI deploying over SSH: GitHub would hold credentials for every server, and ADR-0014 already makes upgrades an ordered operator action.
- Following a branch or `:latest` for the install: the script resolves an exact release tag, so a re-run never picks up unreleased code.
- Automatic updates on by default: an operator must be able to keep the gate, and a compromised GitHub account must not reach every host without an explicit opt-in.
- Signing the Node package: no vendor does it for its agent (ADR-0014), and the checksum covers transport.
- Installing Docker or Podman from the script: distribution package managers differ, and the host's container runtime belongs to its operator.
- Configuring `insecure-registries` on Nodes: it restarts the host's container runtime, and the Control Plane still refuses plain HTTP to a public address.
- Requiring the deployer to bring a reverse proxy: the stack ships one, and an external proxy is only a forwarding hop.
- Putting the registry or the object store under a path other than the protocol's own: Docker clients hard-code `/v2/`, and SigV4 signs the full path and `Host`.
- Reusing `deploy/compose.yaml` for production: its defaults are development credentials and same-host addresses.
