#!/usr/bin/env python3
"""Run the M1 acceptance scenarios through the Python SDK against a deployment.

    ORRERY_SANDBOX_API_URL=http://127.0.0.1:18080 \
    ORRERY_SANDBOX_TOKEN=<token> \
    python sdk/python/tests/parity.py

Scenarios: exec-log-stream (#6) and files-api (#15). On a GPU host, set
ORRERY_PARITY_GPU=1 to also run gpu-sandbox-ready (#3); ORRERY_E2E_GPU_SNAPSHOT
names the platform Snapshot (default `cuda`).

Plain script with checks; no test framework.
"""

from __future__ import annotations

import io
import os
import sys
import tarfile
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from orrery import Exec, Frame, Sandbox, Orrery, OrreryError  # noqa: E402

API_URL = os.environ.get("ORRERY_SANDBOX_API_URL", "")
TOKEN = os.environ.get("ORRERY_SANDBOX_TOKEN", "")


def check(condition: object, message: str) -> None:
    if not condition:
        raise AssertionError(message)


def expect_code(call, code: str) -> None:
    try:
        call()
    except OrreryError as err:
        check(err.code == code, f"expected {code}, got {err.status} {err.code}: {err.detail}")
        return
    raise AssertionError(f"expected {code}, the call succeeded")


def create_ready(client: Orrery, **fields) -> Sandbox:
    sandbox = client.create_sandbox(**fields)
    try:
        ready = client.wait_for_sandbox(sandbox.id, "ready", timeout=300)
        check(ready.state == "ready", f"Sandbox {sandbox.id} is {ready.state}, want ready")
        return ready
    except BaseException:
        client.delete_sandbox(sandbox.id)
        raise


def wait_deleted(client: Orrery, sandbox_id: str, timeout: float = 30.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if client.get_sandbox(sandbox_id).state == "deleted":
                return
        except OrreryError as err:
            if err.status == 404:
                return
            raise
        time.sleep(0.25)
    raise AssertionError(f"Sandbox {sandbox_id} was not deleted within {timeout}s")


def run(client: Orrery, sandbox_id: str, argv: list[str], **fields) -> tuple[Exec, list[Frame]]:
    started = client.create_exec(sandbox_id, argv, **fields)
    frames = list(client.follow_output(started.id, after=0, timeout=120))
    return client.wait_exec(started.id, timeout=60), frames


def stream_text(frames: list[Frame], stream: str) -> bytes:
    return b"".join(f.data_bytes for f in frames if f.stream == stream)


def exec_log_stream(client: Orrery) -> None:
    sandbox = create_ready(client, cpu=0.5, memory=128 << 20, idle_stop=0)
    try:
        finished, frames = run(
            client,
            sandbox.id,
            ["sh", "-c", "printf 'hello '; printf 'oops' >&2; printf '\\377'; exit 7"],
        )
        check(stream_text(frames, "stdout") == b"hello \xff", "stdout did not round-trip the invalid byte")
        check(stream_text(frames, "stderr") == b"oops", "stderr did not arrive separately")
        check(
            any(f.encoding == "base64" and f.data_bytes == b"\xff" for f in frames),
            "the 0xff frame was not marked base64",
        )
        check(finished.state == "finished", f"Exec state is {finished.state}, want finished")
        check(finished.result is not None, "a finished Exec has no result")
        check(finished.result.cause == "exited", f"cause is {finished.result.cause}, want exited")
        check(finished.result.exit_code == 7, f"exit_code is {finished.result.exit_code}, want 7")

        page = client.read_output(finished.id, after=0)
        check(
            [(f.seq, f.stream, f.data, f.encoding) for f in page]
            == [(f.seq, f.stream, f.data, f.encoding) for f in frames],
            "the output page after the Exec finished differs from the followed frames",
        )

        finished, frames = run(client, sandbox.id, ["cat"], stdin="ping\n")
        check(stream_text(frames, "stdout") == b"ping\n", "stdin was not written once and closed")
        check(finished.result is not None and finished.result.cause == "exited", "cat did not exit")

        canceled = client.create_exec(sandbox.id, ["sleep", "60"])
        deadline = time.monotonic() + 15
        while client.get_exec(canceled.id).state != "running":
            check(time.monotonic() < deadline, "the cancelable Exec never became running")
            time.sleep(0.1)
        client.cancel_exec(canceled.id)
        list(client.follow_output(canceled.id, after=0, timeout=60))
        finished = client.wait_exec(canceled.id, timeout=30)
        check(
            finished.result is not None and finished.result.cause == "canceled",
            f"canceled Exec cause is {finished.result and finished.result.cause}, want canceled",
        )
    finally:
        client.delete_sandbox(sandbox.id)
        wait_deleted(client, sandbox.id)


def tar_names(raw: bytes) -> list[str]:
    with tarfile.open(fileobj=io.BytesIO(raw)) as archive:
        return archive.getnames()


def one_file_tar(name: str, data: bytes) -> bytes:
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w") as archive:
        info = tarfile.TarInfo(name)
        info.size = len(data)
        archive.addfile(info, io.BytesIO(data))
    return buffer.getvalue()


def files_api(client: Orrery) -> None:
    sandbox = create_ready(client, cpu=0.5, memory=128 << 20, idle_stop=0)
    try:
        content = b"hello workspace\n"
        client.write_file(sandbox.id, "src/hello.txt", content)
        check(client.read_file(sandbox.id, "src/hello.txt") == content, "upload and download disagree")
        check(
            client.read_file(sandbox.id, "src/hello.txt", offset=6, limit=9) == b"workspace",
            "a Range download returned the wrong bytes",
        )

        entries = client.list_files(sandbox.id, "src")
        check(
            len(entries) == 1
            and entries[0].path == "src/hello.txt"
            and entries[0].type == "file"
            and entries[0].size == len(content),
            f"list = {entries}",
        )
        check(client.stat_file(sandbox.id, "src/hello.txt").type == "file", "stat did not report a file")

        client.mkdir(sandbox.id, "empty")
        client.move_file(sandbox.id, "src/hello.txt", "src/moved.txt")
        check(client.read_file(sandbox.id, "src/moved.txt") == content, "move lost the content")
        client.delete_file(sandbox.id, "src/moved.txt")
        expect_code(lambda: client.read_file(sandbox.id, "src/moved.txt"), "not_found")

        for path in ("../escape.txt", "/etc/passwd", "src/../../escape.txt"):
            expect_code(lambda: client.write_file(sandbox.id, path, b"x"), "invalid_request")
            expect_code(lambda: client.read_file(sandbox.id, path), "invalid_request")

        finished, _ = run(client, sandbox.id, ["ln", "-s", "/etc", "escape"])
        check(finished.result is not None and finished.result.cause == "exited", "ln did not exit")
        expect_code(lambda: client.read_file(sandbox.id, "escape/passwd"), "invalid_request")
        check(client.stat_file(sandbox.id, "escape").type == "symlink", "the symlink itself is not statable")

        client.write_file(sandbox.id, "tree/a.txt", b"alpha")
        archive = client.download_archive(sandbox.id, "tree")
        check(len(tar_names(archive)) >= 2, f"archive has too few entries: {tar_names(archive)}")
        client.upload_archive(sandbox.id, "unpacked", archive)
        check(client.read_file(sandbox.id, "unpacked/tree/a.txt") == b"alpha", "archive upload lost the subtree")
        expect_code(
            lambda: client.upload_archive(sandbox.id, "evil", one_file_tar("../outside.txt", b"x")),
            "invalid_request",
        )
    finally:
        client.delete_sandbox(sandbox.id)
        wait_deleted(client, sandbox.id)


def secrets(client: Orrery) -> None:
    name = f"SDK_PARITY_{os.getpid()}_{int(time.time())}"
    created = client.put_secret(name, "first-value")
    try:
        check(created.name == name, f"put_secret returned {created.name}")
        check(any(item.name == name for item in client.list_secrets().items), "the Secret is not listed")
    finally:
        client.delete_secret(name)
    check(not any(item.name == name for item in client.list_secrets().items), "the Secret is still listed")


def gpu_sandbox_ready(client: Orrery) -> None:
    snapshot = os.environ.get("ORRERY_E2E_GPU_SNAPSHOT", "cuda")
    sandbox = create_ready(
        client,
        cpu=0.5,
        memory=64 << 20,
        idle_stop=0,
        snapshot=snapshot,
        gpu={"count": 1},
    )
    try:
        check(sandbox.gpu is not None and sandbox.gpu.count == 1, f"gpu = {sandbox.gpu}")
        finished, frames = run(
            client,
            sandbox.id,
            ["nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader,nounits"],
        )
        check(finished.result is not None and finished.result.cause == "exited", "nvidia-smi did not exit")
        uuids = [line.strip() for line in stream_text(frames, "stdout").decode().splitlines() if line.strip()]
        check(len(uuids) == 1, f"the Sandbox sees {uuids}, want exactly one card")
    finally:
        client.delete_sandbox(sandbox.id)
        wait_deleted(client, sandbox.id)


def main() -> int:
    check(API_URL, "ORRERY_SANDBOX_API_URL is not set")
    check(TOKEN, "ORRERY_SANDBOX_TOKEN is not set")
    with Orrery(API_URL, TOKEN) as client:
        for name, scenario in (
            ("exec-log-stream", exec_log_stream),
            ("files-api", files_api),
            ("secrets", secrets),
        ):
            scenario(client)
            print(f"ok {name}")
        if os.environ.get("ORRERY_PARITY_GPU") == "1":
            gpu_sandbox_ready(client)
            print("ok gpu-sandbox-ready")
    print("sdk-parity: ok")
    return 0


if __name__ == "__main__":
    sys.exit(main())
