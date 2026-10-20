"""A synchronous client for the Orrery public REST API.

Covers Sandbox lifecycle, Exec, Workspace files and Secrets (api.md §9). It
does not cover Resource upload, Workspace publish, port forwarding or PTY.
"""

from __future__ import annotations

import base64
import time
from dataclasses import dataclass, field, fields
from typing import Any, Iterator
from urllib.parse import quote

import httpx

__all__ = [
    "Exec",
    "ExecResult",
    "FileEntry",
    "Frame",
    "GPU",
    "Page",
    "Sandbox",
    "Secret",
    "Orrery",
    "OrreryError",
    "WorkspaceRef",
]

_TERMINAL_SANDBOX_STATES = {"failed", "deleted"}


class OrreryError(Exception):
    """A non-2xx answer, parsed from the problem+json body (api.md §2)."""

    def __init__(self, status: int, code: str, detail: str):
        self.status = status
        self.code = code
        self.detail = detail
        super().__init__(f"{status} {code}: {detail}" if code else f"{status}: {detail}")


def _q(value: Any) -> str:
    return quote(str(value), safe="")


def _clean(values: dict[str, Any]) -> dict[str, Any]:
    return {key: value for key, value in values.items() if value is not None}


def _fit(cls: type, data: dict[str, Any]):
    names = {f.name for f in fields(cls)}
    return cls(**{key: value for key, value in data.items() if key in names})


@dataclass
class Page:
    items: list[Any]
    next_cursor: str | None = None


@dataclass
class GPU:
    count: int = 0
    model: str | None = None


@dataclass
class WorkspaceRef:
    id: str = ""
    mode: str = "rw"


@dataclass
class Sandbox:
    id: str = ""
    name: str = ""
    labels: dict[str, str] = field(default_factory=dict)
    runtime: str = ""
    state: str = ""
    cause: str | None = None
    node_id: str | None = None
    snapshot: str = ""
    cpu: float | None = None
    memory: int | None = None
    gpu: GPU | None = None
    idle_stop: int | None = None
    conditions: list[str] = field(default_factory=list)
    workspace: WorkspaceRef | None = None
    queue_position: int | None = None
    blocked_reason: str | None = None
    created_at: str | None = None

    @classmethod
    def from_data(cls, data: dict[str, Any]) -> "Sandbox":
        data = dict(data)
        if isinstance(data.get("gpu"), dict):
            data["gpu"] = _fit(GPU, data["gpu"])
        if isinstance(data.get("workspace"), dict):
            data["workspace"] = _fit(WorkspaceRef, data["workspace"])
        return _fit(cls, data)


@dataclass
class ExecResult:
    cause: str = ""
    exit_code: int | None = None
    signal: int | None = None


@dataclass
class Exec:
    id: str = ""
    sandbox_id: str = ""
    argv: list[str] = field(default_factory=list)
    cwd: str = ""
    timeout: int | None = None
    state: str = ""
    result: ExecResult | None = None
    log_truncated: bool = False
    accepted_at: str | None = None
    started_at: str | None = None
    finished_at: str | None = None
    deadline_at: str | None = None

    @classmethod
    def from_data(cls, data: dict[str, Any]) -> "Exec":
        data = dict(data)
        if isinstance(data.get("result"), dict):
            data["result"] = _fit(ExecResult, data["result"])
        return _fit(cls, data)


@dataclass
class Frame:
    """One Exec output frame. `end` is the offset to pass as the next `after`."""

    seq: int = 0
    stream: str = ""
    data: str = ""
    encoding: str | None = None

    @property
    def data_bytes(self) -> bytes:
        if self.encoding == "base64":
            return base64.b64decode(self.data)
        return self.data.encode("utf-8")

    @property
    def end(self) -> int:
        # supervisor.HeaderSize: the stream byte plus a four-byte length.
        return self.seq + 5 + len(self.data_bytes)


@dataclass
class FileEntry:
    path: str = ""
    name: str = ""
    type: str = ""
    size: int = 0
    mode: int = 0
    mod_time: str | None = None
    link_target: str | None = None

    @classmethod
    def from_data(cls, data: dict[str, Any]) -> "FileEntry":
        return _fit(cls, data)


@dataclass
class Secret:
    name: str = ""
    updated_at: str | None = None


class Orrery:
    """Synchronous client. Pass the API server root, e.g. http://127.0.0.1:18080."""

    def __init__(
        self,
        base_url: str,
        token: str,
        *,
        timeout: float = 65.0,
        transport: httpx.BaseTransport | None = None,
    ):
        self._http = httpx.Client(
            base_url=base_url.rstrip("/") + "/api/v1",
            headers={"Authorization": f"Bearer {token}"},
            timeout=timeout,
            transport=transport,
        )

    def close(self) -> None:
        self._http.close()

    def __enter__(self) -> "Orrery":
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    # -- plumbing ---------------------------------------------------------

    def _send(self, method: str, path: str, **kwargs: Any) -> httpx.Response:
        response = self._http.request(method, path, **kwargs)
        if not response.is_success:
            try:
                problem = response.json()
            except ValueError:
                problem = {}
            raise OrreryError(
                response.status_code,
                problem.get("code", ""),
                problem.get("detail") or response.reason_phrase,
            )
        return response

    def _request(self, method: str, path: str, **kwargs: Any) -> Any:
        return self._send(method, path, **kwargs).json()

    # -- Sandbox ----------------------------------------------------------

    def create_sandbox(self, **fields: Any) -> Sandbox:
        """POST /sandboxes. Fields are the CreateSandboxRequest fields."""
        return Sandbox.from_data(self._request("POST", "/sandboxes", json=_clean(fields)))

    def list_sandboxes(
        self,
        *,
        owner: str | None = None,
        state: str | None = None,
        label: str | None = None,
        node: str | None = None,
        has_gpu: bool | None = None,
        limit: int | None = None,
        cursor: str | None = None,
    ) -> Page:
        data = self._request(
            "GET",
            "/sandboxes",
            params=_clean({
                "owner": owner,
                "state": state,
                "label": label,
                "node": node,
                "has_gpu": has_gpu,
                "limit": limit,
                "cursor": cursor,
            }),
        )
        return Page([Sandbox.from_data(item) for item in data["items"]], data.get("next_cursor"))

    def get_sandbox(self, sandbox_id: str, wait_for: str | None = None) -> Sandbox:
        """GET /sandboxes/{id}; wait_for long-polls up to 60 seconds."""
        return Sandbox.from_data(
            self._request(
                "GET",
                f"/sandboxes/{_q(sandbox_id)}",
                params=_clean({"wait_for": wait_for}),
            )
        )

    def wait_for_sandbox(
        self, sandbox_id: str, state: str = "ready", *, timeout: float | None = None
    ) -> Sandbox:
        """Long-poll until the Sandbox reaches `state`, a terminal state, or the timeout."""
        deadline = None if timeout is None else time.monotonic() + timeout
        while True:
            sandbox = self.get_sandbox(sandbox_id, wait_for=state)
            if sandbox.state == state or sandbox.state in _TERMINAL_SANDBOX_STATES:
                return sandbox
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(f"Sandbox {sandbox_id} stayed {sandbox.state}; wanted {state}")

    def _sandbox_action(self, sandbox_id: str, action: str) -> Sandbox:
        return Sandbox.from_data(
            self._request("POST", f"/sandboxes/{_q(sandbox_id)}/{action}")
        )

    def start_sandbox(self, sandbox_id: str) -> Sandbox:
        return self._sandbox_action(sandbox_id, "start")

    def stop_sandbox(self, sandbox_id: str) -> Sandbox:
        return self._sandbox_action(sandbox_id, "stop")

    def delete_sandbox(self, sandbox_id: str) -> Sandbox:
        return Sandbox.from_data(
            self._request("DELETE", f"/sandboxes/{_q(sandbox_id)}")
        )

    # -- Exec -------------------------------------------------------------

    def create_exec(
        self,
        sandbox_id: str,
        argv: list[str],
        *,
        env: dict[str, str] | None = None,
        cwd: str | None = None,
        timeout: int | None = None,
        stdin: str | None = None,
    ) -> Exec:
        body = _clean({
            "argv": list(argv),
            "env": env,
            "cwd": cwd,
            "timeout": timeout,
            "stdin": stdin,
        })
        return Exec.from_data(
            self._request("POST", f"/sandboxes/{_q(sandbox_id)}/execs", json=body)
        )

    def list_execs(
        self, sandbox_id: str, *, limit: int | None = None, cursor: str | None = None
    ) -> Page:
        data = self._request(
            "GET",
            f"/sandboxes/{_q(sandbox_id)}/execs",
            params=_clean({"limit": limit, "cursor": cursor}),
        )
        return Page([Exec.from_data(item) for item in data["items"]], data.get("next_cursor"))

    def get_exec(self, exec_id: str) -> Exec:
        return Exec.from_data(self._request("GET", f"/execs/{_q(exec_id)}"))

    def cancel_exec(self, exec_id: str) -> Exec:
        return Exec.from_data(self._request("POST", f"/execs/{_q(exec_id)}/cancel"))

    def wait_exec(self, exec_id: str, *, timeout: float | None = None, poll: float = 0.1) -> Exec:
        """Poll until the Exec is finished; `timeout` is only this call's patience."""
        deadline = None if timeout is None else time.monotonic() + timeout
        while True:
            current = self.get_exec(exec_id)
            if current.state == "finished":
                return current
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(f"Exec {exec_id} stayed {current.state}")
            time.sleep(poll)

    def read_output(self, exec_id: str, after: int | None = None) -> list[Frame]:
        """One page of output; `after` is the previous frame's `end`."""
        data = self._request(
            "GET",
            f"/execs/{_q(exec_id)}/output",
            params=_clean({"after": after}),
        )
        return [_fit(Frame, item) for item in data]

    def follow_output(
        self,
        exec_id: str,
        after: int | None = None,
        *,
        timeout: float | None = None,
        poll: float = 0.1,
    ) -> Iterator[Frame]:
        """Yield output frames in order until the Exec finishes."""
        deadline = None if timeout is None else time.monotonic() + timeout
        while True:
            frames = self.read_output(exec_id, after)
            for frame in frames:
                yield frame
                after = frame.end
            if frames:
                continue
            if self.get_exec(exec_id).state == "finished":
                return
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(f"Exec {exec_id} did not finish")
            time.sleep(poll)

    # -- Files ------------------------------------------------------------

    def _files_path(self, sandbox_id: str, suffix: str = "") -> str:
        return f"/sandboxes/{_q(sandbox_id)}/files{suffix}"

    def list_files(self, sandbox_id: str, path: str = "") -> list[FileEntry]:
        data = self._request("GET", self._files_path(sandbox_id), params={"path": path})
        return [FileEntry.from_data(item) for item in data["items"]]

    def stat_file(self, sandbox_id: str, path: str) -> FileEntry:
        data = self._request(
            "GET", self._files_path(sandbox_id, "/stat"), params={"path": path}
        )
        return FileEntry.from_data(data)

    def read_file(
        self,
        sandbox_id: str,
        path: str,
        *,
        offset: int | None = None,
        limit: int | None = None,
    ) -> bytes:
        """Download a file; offset and limit use HTTP Range."""
        headers = {}
        if offset is not None or limit is not None:
            start = offset or 0
            end = "" if limit is None else str(start + limit - 1)
            headers["Range"] = f"bytes={start}-{end}"
        response = self._send(
            "GET",
            self._files_path(sandbox_id, "/content"),
            params={"path": path},
            headers=headers,
        )
        return response.content

    def write_file(self, sandbox_id: str, path: str, data: bytes | str) -> None:
        self._send(
            "PUT",
            self._files_path(sandbox_id, "/content"),
            params={"path": path},
            content=data,
        )

    def mkdir(self, sandbox_id: str, path: str) -> None:
        self._send(
            "POST", self._files_path(sandbox_id, "/mkdir"), json={"path": path}
        )

    def move_file(self, sandbox_id: str, source: str, destination: str) -> None:
        self._send(
            "POST",
            self._files_path(sandbox_id, "/move"),
            json={"from": source, "to": destination},
        )

    def delete_file(self, sandbox_id: str, path: str) -> None:
        self._send(
            "DELETE", self._files_path(sandbox_id), params={"path": path}
        )

    def download_archive(self, sandbox_id: str, path: str) -> bytes:
        """Download a Workspace subtree as tar (api.md §5)."""
        return self._send(
            "GET", self._files_path(sandbox_id, "/archive"), params={"path": path}
        ).content

    def upload_archive(self, sandbox_id: str, path: str, data: bytes) -> None:
        """Unpack a tar into the Workspace at `path`."""
        self._send(
            "PUT",
            self._files_path(sandbox_id, "/archive"),
            params={"path": path},
            content=data,
        )

    # -- Secrets ----------------------------------------------------------

    def put_secret(self, name: str, value: str) -> Secret:
        return _fit(
            Secret,
            self._request("PUT", f"/secrets/{_q(name)}", json={"value": value}),
        )

    def list_secrets(
        self, *, limit: int | None = None, cursor: str | None = None
    ) -> Page:
        data = self._request(
            "GET", "/secrets", params=_clean({"limit": limit, "cursor": cursor})
        )
        return Page([_fit(Secret, item) for item in data["items"]], data.get("next_cursor"))

    def delete_secret(self, name: str) -> Secret:
        return _fit(Secret, self._request("DELETE", f"/secrets/{_q(name)}"))
