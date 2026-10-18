package client

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"time"
)

// frameHeaderSize is the Supervisor's record framing: one stream byte plus a
// four-byte big-endian length (supervisor.HeaderSize). It stays a constant
// here so the public package depends on the standard library only.
const frameHeaderSize = 5

// RunExec creates an Exec. When wait is set it also follows the output to
// completion and returns the finished Exec; the output is discarded, so use
// CreateExec and FollowOutput when the caller needs the bytes.
func (c *Client) RunExec(ctx context.Context, sandboxID string, req ExecRequest, wait bool) (*Exec, error) {
	created, err := c.CreateExec(ctx, sandboxID, req, "")
	if err != nil || !wait {
		return created, err
	}
	return c.FollowOutput(ctx, created.ID, io.Discard, io.Discard)
}

// FollowOutput polls output pages until a page is empty and the Exec is
// finished, writing frames as they arrive. It is the CLI's original follow
// loop (api.md §4).
// ponytail: polls instead of the follow WebSocket; reuse the socket if 100 ms
// latency matters.
func (c *Client) FollowOutput(ctx context.Context, execID string, stdout, stderr io.Writer) (*Exec, error) {
	var after int64
	for {
		frames, err := c.ExecOutput(ctx, execID, after)
		if err != nil {
			return nil, err
		}
		for _, f := range frames {
			after = writeFrame(stdout, stderr, f)
		}
		if len(frames) > 0 {
			continue
		}
		exec, err := c.GetExec(ctx, execID)
		if err != nil {
			return nil, err
		}
		if exec.State == "finished" {
			return exec, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// writeFrame writes one output frame and returns the next byte offset.
func writeFrame(stdout, stderr io.Writer, f Frame) int64 {
	data := []byte(f.Data)
	if f.Encoding == "base64" {
		if raw, err := base64.StdEncoding.DecodeString(f.Data); err == nil {
			data = raw
		}
	}
	switch f.Stream {
	case "stderr":
		_, _ = stderr.Write(data)
	case "truncated":
		fmt.Fprintln(stderr, "\n[output truncated]")
	default:
		_, _ = stdout.Write(data)
	}
	return f.Seq + frameHeaderSize + int64(len(data))
}
