"""Unit checks for the SDK's wire contract, run with plain unittest.

    python -m unittest discover -s sdk/python/tests
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

import httpx

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from stars_sandbox import StarsSandbox, StarsSandboxError  # noqa: E402


def client(handler) -> StarsSandbox:
    return StarsSandbox("http://api", "tok", transport=httpx.MockTransport(handler))


class ErrorTest(unittest.TestCase):
    def test_problem_json_becomes_stars_sandbox_error(self) -> None:
        def handler(request: httpx.Request) -> httpx.Response:
            return httpx.Response(409, json={"code": "conflict", "detail": "the Sandbox is not ready"})

        with self.assertRaises(StarsSandboxError) as caught:
            client(handler).get_sandbox("sb-1")
        err = caught.exception
        self.assertEqual((err.status, err.code, err.detail), (409, "conflict", "the Sandbox is not ready"))


class SandboxTest(unittest.TestCase):
    def test_wait_for_is_sent_and_polling_stops_at_the_wanted_state(self) -> None:
        states = iter(["pending", "pending", "ready"])
        waited = []

        def handler(request: httpx.Request) -> httpx.Response:
            waited.append(request.url.params.get("wait_for"))
            return httpx.Response(200, json={"id": "sb-1", "state": next(states)})

        sandbox = client(handler).wait_for_sandbox("sb-1", "ready", timeout=10)
        self.assertEqual(sandbox.state, "ready")
        self.assertEqual(waited, ["ready", "ready", "ready"])


class ExecOutputTest(unittest.TestCase):
    def test_follow_output_advances_by_frame_end_and_decodes_base64(self) -> None:
        requested = []

        def handler(request: httpx.Request) -> httpx.Response:
            if request.url.path.endswith("/output"):
                after = int(request.url.params["after"])
                requested.append(after)
                if after == 0:
                    # "hi" ends at 0 + 5 + 2; the invalid byte is one byte at 7.
                    return httpx.Response(200, json=[
                        {"seq": 0, "stream": "stdout", "data": "hi"},
                        {"seq": 7, "stream": "stdout", "data": "/w==", "encoding": "base64"},
                    ])
                return httpx.Response(200, json=[])
            return httpx.Response(200, json={"id": "e-1", "state": "finished",
                                             "result": {"cause": "exited", "exit_code": 7}})

        frames = list(client(handler).follow_output("e-1", after=0))
        self.assertEqual([f.data_bytes for f in frames], [b"hi", b"\xff"])
        self.assertEqual([f.end for f in frames], [7, 13])
        self.assertEqual(requested, [0, 13])


class FilesTest(unittest.TestCase):
    def test_list_and_stat_files_decode_entries(self) -> None:
        seen = {}

        def handler(request: httpx.Request) -> httpx.Response:
            seen[(request.method, request.url.path)] = request.url.params.get("path")
            if request.url.path.endswith("/files/stat"):
                return httpx.Response(200, json={
                    "path": "src/a.txt", "name": "a.txt", "type": "file", "size": 6,
                    "mode": 420, "mod_time": "2026-10-18T00:00:00Z", "link_target": None,
                })
            return httpx.Response(200, json={"items": [
                {"path": "src/a.txt", "name": "a.txt", "type": "file", "size": 6,
                 "mode": 420, "mod_time": "2026-10-18T00:00:00Z"},
                {"path": "src/link", "name": "link", "type": "symlink", "size": 0,
                 "mode": 511, "link_target": "a.txt"},
            ]})

        c = client(handler)
        entries = c.list_files("sb", "src")
        self.assertEqual(
            [(e.path, e.type, e.size) for e in entries],
            [("src/a.txt", "file", 6), ("src/link", "symlink", 0)],
        )
        self.assertEqual(entries[1].link_target, "a.txt")
        entry = c.stat_file("sb", "src/a.txt")
        self.assertEqual((entry.path, entry.type, entry.size, entry.mode),
                         ("src/a.txt", "file", 6, 420))
        self.assertEqual(seen, {
            ("GET", "/api/v1/sandboxes/sb/files"): "src",
            ("GET", "/api/v1/sandboxes/sb/files/stat"): "src/a.txt",
        })

    def test_content_and_archive_round_trip(self) -> None:
        seen = {}

        def handler(request: httpx.Request) -> httpx.Response:
            seen[(request.method, request.url.path)] = (
                request.url.params.get("path"), request.headers.get("range"), request.content,
            )
            if request.url.path.endswith("/archive"):
                return httpx.Response(200, content=b"tar-bytes")
            if request.method == "PUT":
                return httpx.Response(204)
            return httpx.Response(206, content=b"workspace")

        c = client(handler)
        c.write_file("sb", "src/hello.txt", b"hello workspace\n")
        self.assertEqual(c.read_file("sb", "src/hello.txt", offset=6, limit=9), b"workspace")
        self.assertEqual(c.download_archive("sb", "tree"), b"tar-bytes")
        c.upload_archive("sb", "unpacked", b"tar-bytes")

        self.assertEqual(seen[("PUT", "/api/v1/sandboxes/sb/files/content")][2], b"hello workspace\n")
        self.assertEqual(seen[("GET", "/api/v1/sandboxes/sb/files/content")][1], "bytes=6-14")
        archive_get = seen[("GET", "/api/v1/sandboxes/sb/files/archive")]
        self.assertEqual(archive_get[:2], ("tree", None))
        self.assertEqual(seen[("PUT", "/api/v1/sandboxes/sb/files/archive")][2], b"tar-bytes")


class SecretTest(unittest.TestCase):
    def test_secret_name_is_path_escaped(self) -> None:
        paths = []

        def handler(request: httpx.Request) -> httpx.Response:
            paths.append(request.url.raw_path.decode())
            return httpx.Response(200, json={"name": "A B", "updated_at": "2026-10-18T00:00:00Z"})

        client(handler).put_secret("A B", "value")
        client(handler).delete_secret("A B")
        self.assertEqual(paths, ["/api/v1/secrets/A%20B", "/api/v1/secrets/A%20B"])


if __name__ == "__main__":
    unittest.main()
