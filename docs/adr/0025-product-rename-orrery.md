# ADR-0025: Rename the platform to Orrery

- Status: accepted
- Date: 2026-10-20

## Context

Issue #149. The project was named `stars-sandbox`, a descriptive name that collided with every other "sandbox" project and carried no identity of its own. The product is now named **Orrery**: a mechanical model that drives the orbits of a system's bodies, which is the same shape as a Control Plane that decides and a set of Node Agents that execute. The old name had leaked into the repository name and Go module, the binaries, the proto package, environment variables, host and in-Sandbox paths, Docker labels and cgroup units, cookies, token prefixes, the Python SDK and deployment defaults.

## Decision

**Rename every product-owned identifier to Orrery.** The mapping is:

- Repository and Go module: `zibyn/stars-sandbox` → `zibyn/orrery`.
- Binaries: `stars-sandbox-server`, `stars-sandbox-agent`, `stars-sandbox` → `orrery-server`, `orrery-agent`, `orrery`; release assets `stars-sandbox-node_*` and `stars-sandbox_*` → `orrery-node_*` and `orrery_*`.
- Proto package: `stars.node.v1` → `orrery.node.v1`, and therefore the gRPC path `/orrery.node.v1.Node/`.
- Environment variables: `STARS_*` → `ORRERY_*`.
- Paths: `/.stars/bin` and `/.stars/run` → `/.orrery/bin` and `/.orrery/run`; `/etc/stars-sandbox[-agent]` and `/var/lib/stars-sandbox*` → the corresponding `orrery` paths.
- Docker labels, annotations, cgroup slices and systemd units: `stars.*`, `stars-sb-*`, `stars-exec-*`, `stars-build-*` → `orrery.*` and `orrery-*`.
- Cookies: `stars_session`, `stars_port`, `stars_oidc_state` → `orrery_session`, `orrery_port`, `orrery_oidc_state`.
- Token prefixes: `ssk_` (API), `ssn_` (Node), `sse_` (Enrollment Packet), `sst_` (Sandbox) → `orr_`, `orn_`, `ore_`, `ors_`.
- Python SDK: package `stars_sandbox`, class `StarsSandbox` and error `StarsSandboxError` → `orrery`, `Orrery`, `OrreryError`.
- Deployment defaults: the PostgreSQL user and database, the S3 bucket and access key, the registry user, and the development credentials `stars` / `stars-secret` → `orrery` / `orrery-secret`.

**No compatibility aliases.** No release has been published, so there are no deployed Agents or saved browser sessions to migrate. The Control Plane and every Node Agent must run versions from the same side of this rename.

**Not renamed.** The external domain `registry.starsdom.com`, test payloads that merely contain the word `stars`, and GitHub star counts quoted in research notes are not product identifiers.

## Consequences

- The node protocol is wire-incompatible across the rename: an old Agent cannot register with a new Control Plane, and vice versa. Service paths, environment variables, cookies and on-disk locations all move together, so a deployment cannot be upgraded in place; it is reinstalled under the new names.
- GitHub redirects the old repository URLs, but the Go module path, `install.sh`'s `REPO`, documentation raw URLs and release asset names are new.
- The specification, ADRs and generated protobuf code now use the new names; `buf generate` reproduces the generated files from the renamed proto package.

## Rejected alternatives

- Keep the old internal identifiers and change only the product name: two vocabularies for one system, and every future operator and agent would have to translate between them.
- Read both `STARS_*` and `ORRERY_*`, or accept both proto packages: dead compatibility code for a platform with no released version.
- Rename only the code and leave the GitHub repository: the module path and the repository would disagree.
