# ADR-0019: One Exec Supervisor per Exec, a file record, and a control socket

- Status: accepted
- Date: 2026-10-17

## Context

`#43` had to make ADR-0009's Exec Supervisor concrete for Docker, Podman and the Local Runtime (`docs/spec/runtime.md`, facts in `docs/research/runtime-adapter-mechanics.md`). Daytona runs one daemon per sandbox as its entrypoint and writes command output and exit codes to files. Coder runs its agent as the entrypoint and keeps PTYs in a ring buffer.

## Decision

- **One Supervisor process per Exec**, started by a detached exec, plus the same binary as an idle PID 1 that only reaps. There is no per-Sandbox daemon and no Agent-to-daemon RPC. Coder splits PID 1 the same way; Daytona's single process races its reaper against `cmd.Wait()`.
- **The record is files on a second platform mount**, `/.stars/run`, read-write and per Sandbox. It holds an append-only frame file whose `seq` is the frame's byte offset, and atomically written `started` and `exit` files. The Sandbox's own processes can tamper with it; it is not a boundary against them.
- **The Exec specification, Secrets included, arrives over a per-Exec unix socket**, never through the exec's Env or argv. Podman persists exec Env in its state database, Docker exposes argv through inspect, and `systemctl show` exposes `systemd-run -E`. The same socket relays cancel, PTY input and resize, and serves PTY output.
- **The Supervisor alone enforces the timeout and the PTY idle reap**, so both still apply while the Agent is gone.
- **The result is the last frame on `ExecOutput`**, for PTY Execs too. Its `OutputAck` is what lets the Agent delete the record.
- **An Exec ends when its main process exits.** Background children are not killed; only timeout and cancel signal the process group.
- **A Supervisor that died while its Runtime lives ends its Exec as `runtime_lost`**; no new cause.

## Consequences

- An Exec cannot start without the Agent present to send its spec; one interrupted mid-start ends `start_failed` and is retryable.
- `oom_killed` uses the container-wide `oom_kill` counter, as Docker does, so two Execs killed together are not told apart.
- Docker and Podman share one Engine API client. Version support is decided by the capability self-check, not a version table.

## Rejected alternatives

- A per-Sandbox daemon (Daytona): it needs its own protocol, and one crash ends every Exec.
- Records inside the container's rootfs (Daytona): reading them back would need an exec stream again.
- FIFOs for input (Daytona): they are one-way, and PTY output, many attachments and idle counting all need a connection.
- A sub-cgroup per Exec for exact OOM attribution: it needs a writable cgroupfs, which ADR-0009 does not grant.
