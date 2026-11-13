# third_party

`go-fuse` is github.com/hanwen/go-fuse v2.12.1 with `go-fuse-handover.patch`
applied (ADR-0030): `NewServerFromFD`, `SessionFile`, `StopReading`,
`LoopsDone` and `NegotiatedFlags`, so `orrery-agent resourcefs` can hand a
live `/dev/fuse` session to its successor. `go.mod` points at it with
`replace`. Drop the copy once the patch lands upstream.
