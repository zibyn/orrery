# Agent Sandbox Platform

This context defines the resources and trust boundaries for agent-driven remote development and GPU execution.

## Resources and execution

**Sandbox**:
A short-lived isolated execution instance that runs commands and interactive sessions against a Workspace and optional Resources. It does not own their long-term lifecycle.
_Avoid_: Workspace, container (when referring to the platform resource)

**Workspace**:
A mutable working directory for an agent. It may contain arbitrary files and zero or more Git repositories, and is permanently bound to one compute Node for its lifetime. It is either persistent, deleted only explicitly, or ephemeral, created with one Sandbox and deleted with it. It may become temporarily unavailable or permanently lost with that Node. A Workspace is not owned by a Sandbox; multiple Sandboxes may access it subject to their attachment mode and the single-writer rule.
_Avoid_: Sandbox filesystem

**Workspace Attachment**:
The relationship between a Sandbox and a Workspace, with either read-only or read-write access. An attachment does not transfer ownership of the Workspace.
_Avoid_: Workspace lease

**Resource**:
A stable named collection of persisted content whose current version may be mounted, copied, or referenced by execution.
_Avoid_: Workspace, build output

**ResourceVersion**:
An immutable content-addressed version of a Resource. Its address identifies the content; provenance and metadata explain how that content entered the platform.
_Avoid_: mutable file, Workspace snapshot

**Snapshot**:
An immutable, reusable software-environment template. It excludes Workspace state, Resources, Secrets, and running processes.
_Avoid_: Workspace checkpoint, process checkpoint

**SnapshotVersion**:
An immutable image of a Snapshot, built from a base and declared build inputs or imported by digest. Its inputs are traceable; rebuilding them is not promised to yield the same result.
_Avoid_: Workspace snapshot, running Sandbox

**Platform Snapshot**:
A Snapshot given by deployment configuration and readable by every User. It is a recommended starting point for Sandboxes and builds, not a restriction.
_Avoid_: platform base image, base layer, approved image

**Content Address**:
A stable identifier derived from immutable content, used to refer to that content independently of a Resource name or provenance record.
_Avoid_: mutable Resource name

## Execution and infrastructure

**Exec**:
A durable execution intent representing one requested command or interactive operation.
_Avoid_: terminal session, process

**Idempotency Key**:
A caller-supplied identity for one requested operation, used to prevent duplicate durable execution intents.
_Avoid_: retry token

**Cancellation**:
A durable request to stop an Exec; it is distinct from the eventual observation that the execution has stopped.
_Avoid_: immediate termination

**Timeout**:
A platform-enforced execution deadline that produces a timed-out terminal result when the command does not finish in time.
_Avoid_: cancellation (when the cause matters)

**Execution Log**:
The ordered, durable output record of a non-interactive Exec, resumable by sequence and distinct from a live stream attachment. A PTY's output is not an Execution Log.
_Avoid_: terminal session

**PTY Attachment**:
A live input/output connection to an Exec running in PTY mode. Reattaching observes the same execution and does not start another one. Any number of attachments may send input; a PTY left with no attachment for its idle period is terminated.
_Avoid_: terminal session

**Log Spool**:
The Node-local record of Exec output kept until the Control Plane has stored it. It survives a Node Agent restart, preserves ordering, and ends in an explicit truncation marker rather than silent loss when capacity is exhausted.
_Avoid_: best-effort logs

**Exec Supervisor**:
The platform process inside a Sandbox's Runtime that starts an Exec's command and records its output and exit status on the Node, so the Exec outlives a Node Agent restart.
_Avoid_: init process, sidecar, daemon

**Execution Result**:
The durable outcome of an Exec, including its platform cause and, when available, process exit code or terminating signal.
_Avoid_: transport status

**Grace Period**:
The bounded interval after cancellation or timeout during which an Exec may exit normally before forced termination.
_Avoid_: execution timeout

**Reconciler**:
The control-plane process that compares durable desired state with Node-observed state and drives idempotent actions until they converge.
_Avoid_: event handler

**Node Unreachable**:
A condition in which the Control Plane cannot currently confirm Node Agent state; it is not itself proof that a running Exec stopped, nor that the Node is lost. Only an administrator declares a Node lost.
_Avoid_: Exec failure, Node lost

**Log Retention Policy**:
The rule that determines which ordered portions of an Execution Log remain resumable or available as a snapshot.
_Avoid_: log deletion event

**Output Frame**:
One ordered piece of Exec output tagged with its source stream, such as stdout, stderr, or PTY output.
_Avoid_: WebSocket message

**Termination Confirmation**:
An observation that an Exec process is no longer running, distinct from sending a cancellation or kill request.
_Avoid_: cancellation request

**Idle Stop**:
The automatic stopping of a Sandbox that has had no running Exec and no live connection for its idle period.
_Avoid_: hibernation, eviction

**Desired State**:
The lifecycle a caller asked a resource to reach. The Reconciler derives commands from it; it is an input to reconciliation, not an observation of what exists.
_Avoid_: target state, requested status

**Sandbox State**:
The lifecycle condition of a Sandbox independently of its Runtime instance: pending, provisioning, ready, stopping, stopped, deleting, deleted, or failed with a cause. A stopped Sandbox can be started again; an unreachable Node is an unavailable condition, not a state.
_Avoid_: Runtime state

**Node**:
A registered compute host whose Node Agent reports its Runtime Provider, hardware, and capabilities.
_Avoid_: worker container, Sandbox

**Paused Node**:
A Node that receives no new Workspaces or builds, while Workspaces already bound to it can still run Sandboxes and running Sandboxes continue undisturbed. A paused Node with no running Sandbox is drained; drained is a fact, not a separate state.
_Avoid_: cordoned, draining state, evicting

**Node Agent**:
The trusted process on a Node that connects outbound to the Control Plane and performs authorized runtime, file, and execution operations.
_Avoid_: Runner, Docker daemon

**Control Plane**:
The platform authority for identity, resource state, scheduling, leases, lifecycle commands, and audit records.
_Avoid_: Node Agent, data plane

**Data Plane**:
The authorized path for high-frequency operations inside a Sandbox, such as files, Git, Exec output, and PTY traffic. It carries operations that have already been authorized by the Control Plane and does not create independent durable execution intent.
_Avoid_: Control Plane

**Gateway**:
The authorized service boundary that connects callers to a Sandbox Data Plane, including live Exec output and PTY traffic. It does not become a second source of lifecycle or execution semantics.
_Avoid_: Node Agent, Control Plane

**Runtime Provider**:
An adapter that realizes Sandbox and Exec contracts using Docker, Podman, or a trusted Local Runtime.
_Avoid_: container runtime (when referring to the platform abstraction)

**Local Runtime**:
A Runtime Provider that executes in the Node's host environment without container namespace isolation; it is usable only where the Node is trusted for that execution.
_Avoid_: fake runtime, test-only runtime

**Capability**:
A declared operation or security property that a Node/Runtime Provider can enforce; unsupported capabilities must fail explicitly.
_Avoid_: best-effort feature

**Runtime Capabilities**:
The facts a Node Agent self-checks and reports about what a Runtime Provider can enforce on that Node now. Each capability is `enforced`, `best_effort`, or `unsupported`; the Scheduler uses these facts for placement.
_Avoid_: provider promise, static feature list

**Lease**:
A fenced claim over a mutable Workspace or schedulable compute resource. It ends by an explicit act, never by expiry; its fencing token rejects work from a holder that has been superseded. Workspace Attachment and Resource Allocation generations are monotonic counters, not fences: the Control Plane is their only writer. Admission compares `sandboxes.generation` as its CAS guard.
_Avoid_: timed lock, TTL

**User**:
A human or automation principal that owns resources and acts through scoped credentials.
_Avoid_: Tenant, global administrator

## Access and reachability

**API Token**:
A named credential a User presents to the Control Plane or the Gateway. A User may hold several; each is scoped to that User and to a level of authority, and is revoked on its own.
_Avoid_: login, key pair

**Session**:
A browser's credential, obtained by a User logging in with a password or through the external OIDC issuer. It is a kind of API Token that lapses when left idle.
_Avoid_: cookie, login token

**Web console**:
The browser client of the public API and the Gateway. It has no semantics of its own, so nothing is possible only through it.
_Avoid_: dashboard, admin UI

**Enrollment Packet**:
A single-use, short-lived credential by which an unregistered Node obtains its Node Token. Redeeming it is the platform's only unauthenticated entry point.
_Avoid_: join token, registration token, bootstrap secret

**Node Token**:
The credential by which an enrolled Node Agent authenticates to the Control Plane. Deleting the Node or declaring it lost revokes it.
_Avoid_: certificate, runner key

**Port Forward**:
An owner-or-admin connection from outside to a port a process listens on inside a Sandbox, made through the Gateway without exposing the Sandbox on a network address.
_Avoid_: preview URL, public link, ingress

**Secret**:
Caller-originated credential material that the platform holds for a User and may deliver into the User's Sandboxes. Its value can be written but never read back.
_Avoid_: environment variable, config value, password

**Git Provider**:
A platform-configured Git hosting service, public or self-hosted, through which Users may link accounts.
_Avoid_: Repository, remote

**Git Account Link**:
A User's authorization of the platform to act for that User's account on one Git Provider. Git inside the User's Sandboxes authenticates through it.
_Avoid_: Repository, deploy key, PAT

**Sandbox Token**:
A credential given to one Sandbox at creation by which code inside it asks the platform for its owner's Git access, and nothing else. It ends with the Sandbox.
_Avoid_: agent token, API Token

**Audit Record**:
An append-only statement that a platform mutation happened, recorded after the change and kept for a configured period.
_Avoid_: log line, telemetry event, metric

**Deletion**:
The removal of a resource, preceded by a logical deletion that withholds it while references remain. It is authorized by ownership, not by role.
_Avoid_: purge, cleanup, garbage collection

## Scheduling

**Scheduler**:
The Control Plane authority that chooses a Node and admits a Sandbox's requested compute resources.
_Avoid_: Node Agent, Runtime Provider

**Resource Allocation**:
The committed assignment of requested compute resources to one Sandbox on one Node.
_Avoid_: Reservation, quota

**Resource Reservation**:
A held amount of compute capacity associated with an accepted request before the Sandbox is ready.
_Avoid_: Allocation, lease

**GPU UUID**:
The stable platform-visible identity of one physical GPU on a Node.
_Avoid_: GPU index, device ordinal

**User Quota**:
A User-specific upper bound on the GPUs and concurrent Sandboxes they may have accepted or allocated.
_Avoid_: priority, billing limit
