# Agent Sandbox Platform

This context defines the resources and trust boundaries for agent-driven remote development and GPU execution.

## Resources and execution

**Sandbox**:
A short-lived isolated execution instance that runs commands and interactive sessions against a Workspace and optional Resources. It does not own their long-term lifecycle.
_Avoid_: Workspace, container (when referring to the platform resource)

**Workspace**:
A mutable working directory for an agent. It may contain arbitrary files and zero or more Git repositories, and is permanently bound to one compute Node for its lifetime. It can be ephemeral or persistent, and may become temporarily unavailable or permanently lost with that Node. A Workspace is not owned by a Sandbox; multiple Sandboxes may access it subject to their attachment mode and the single-writer rule.
_Avoid_: Sandbox filesystem

**Workspace Attachment**:
The relationship between a Sandbox and a Workspace, with either read-only or read-write access. An attachment does not transfer ownership of the Workspace.
_Avoid_: Workspace lease

**Resource**:
A stable named collection of persisted content whose current version may be mounted, copied, or referenced by execution.
_Avoid_: Workspace, Artifact

**ResourceVersion**:
An immutable content-addressed version of a Resource. Its address identifies the content; provenance and metadata explain how that content entered the platform.
_Avoid_: mutable file, Workspace snapshot

**Artifact**:
A caller-declared output of an Exec that is collected as a ResourceVersion once the Exec has finished; Workspace changes are not Artifacts unless explicitly collected.
_Avoid_: automatic Workspace backup

**Snapshot**:
An immutable, reusable software-environment template. It excludes Workspace state, Resources, Secrets, and running processes.
_Avoid_: Workspace checkpoint, process checkpoint

**SnapshotVersion**:
An immutable build result of a Snapshot, produced from a Platform Base Image and declared build inputs. Its inputs are traceable; rebuilding them is not promised to yield the same result.
_Avoid_: Workspace snapshot, running Sandbox

**Platform Base Image**:
A software environment the platform has approved as the starting point of a Snapshot. Only the platform adds or removes one.
_Avoid_: base layer, user image

**Content Address**:
A stable identifier derived from immutable content, used to refer to that content independently of a Resource name or provenance record.
_Avoid_: mutable Resource name

## Execution and infrastructure

**Exec**:
A durable execution intent representing one requested command or interactive operation.
_Avoid_: terminal session, process

**ExecAttempt**:
One concrete attempt to realize an Exec on a Node through a Runtime Provider. An attempt may become `lost` when execution may have started but its result cannot be confirmed.
_Avoid_: Exec (when referring to the user intent)

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
The ordered, durable output record of an Exec, resumable by sequence and distinct from a live stream attachment.
_Avoid_: terminal session

**PTY Attachment**:
A live input/output connection to an Exec running in PTY mode. Reattaching observes the same execution and does not start another one. At most one attachment controls input at a time; other attachments are observers.
_Avoid_: ExecAttempt

**Execution Retry**:
A new ExecAttempt for the same Exec, allowed only for a confirmed pre-start transient failure and bounded by the platform retry policy.
_Avoid_: duplicate Exec

**Log Spool**:
A bounded temporary record of Exec output held while durable log storage is unavailable; it preserves ordering but does not permit silent loss when capacity is exhausted.
_Avoid_: best-effort logs

**Attempt Identity**:
The stable identity of one ExecAttempt across command delivery, Node Agent restarts, reconciliation, and late status reports.
_Avoid_: delivery attempt

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
A condition in which the Control Plane cannot currently confirm Node Agent state; it is not itself proof that a running Exec stopped.
_Avoid_: Exec failure

**Log Retention Policy**:
The rule that determines which ordered portions of an Execution Log remain resumable or available as a snapshot.
_Avoid_: log deletion event

**Output Frame**:
One ordered piece of Exec output tagged with its source stream, such as stdout, stderr, or PTY output.
_Avoid_: WebSocket message

**Exec Result Aggregation**:
The rule that derives one Exec outcome from its ordered ExecAttempts; a successful Attempt wins while prior failures remain history.
_Avoid_: last-attempt result

**Termination Confirmation**:
An observation that an Exec process is no longer running, distinct from sending a cancellation or kill request.
_Avoid_: cancellation request

**Sandbox State**:
The lifecycle condition of a Sandbox independently of its Runtime instance: provisioning, ready, stopping, stopped, deleting, or deleted; temporary Runtime or Node failure is represented as an unavailable condition.
_Avoid_: Runtime state

**Node**:
A registered compute host whose Node Agent reports available Runtime Providers, hardware, and capabilities.
_Avoid_: worker container, Sandbox

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
A time-bounded, fenced claim over a mutable Workspace or schedulable compute resource.
_Avoid_: permanent lock

**User**:
A human or automation principal that owns resources and acts through scoped credentials.
_Avoid_: Tenant, global administrator

## Access and reachability

**API Token**:
A credential a User presents to the Control Plane or the Gateway. It is scoped to one User and to a level of authority.
_Avoid_: session, login, key pair

**Enrollment Packet**:
A single-use, short-lived credential by which an unregistered Node obtains its first Node Identity. Redeeming it is the platform's only unauthenticated entry point.
_Avoid_: join token, registration token, bootstrap secret

**Secret**:
Caller-originated credential material that the platform holds for a User and may deliver into the User's Sandboxes. Its value can be written but never read back.
_Avoid_: environment variable, config value, password

**Git Provider**:
A platform-configured Git hosting service, public or self-hosted, through which Users may link accounts.
_Avoid_: Repository, remote

**Git Account Link**:
A User's authorization of the platform to act for that User's account on one Git Provider. Git inside the User's Sandboxes authenticates through it.
_Avoid_: Repository, deploy key, PAT

**Capability Channel**:
The platform-managed path by which a Sandbox asks the platform to perform a capability it cannot perform itself. It carries declared platform verbs and their targets, never arbitrary commands. Each Sandbox has its own, a request on it speaks for that Sandbox, and it is independent of the Sandbox's Egress Policy.
_Avoid_: agent socket, proxy, sidecar

**Egress Policy**:
The declared reachability of a Sandbox's network egress, expressed as a tier plus the destinations the caller requests beyond it.
_Avoid_: firewall rules, allowlist, network config

**Egress Opening**:
A platform-recorded permission for a normally denied destination to become reachable. A caller may request one but never grant it.
_Avoid_: exception, whitelist entry, firewall hole

**Audit Record**:
An append-only statement that a platform decision or mutation happened, kept alongside the state it describes.
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
A User-specific upper bound on the resources and capacity they may have accepted or allocated.
_Avoid_: priority, billing limit
