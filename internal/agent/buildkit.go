package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/moby/buildkit/client"
)

// buildkitClient connects to the Node's standalone buildkitd (ADR-0021). With
// no --buildkit address there is no endpoint and snapshot_build stays
// unsupported (runtime.md §12).
func (r *Runtime) buildkitClient(ctx context.Context) (*client.Client, error) {
	if r.buildkit == "" {
		return nil, errors.New("no BuildKit endpoint configured")
	}
	return client.New(ctx, r.buildkit)
}

// buildkitAvailable is the snapshot_build capability self-check: a real RPC
// against the configured BuildKit endpoint, independent of the Node's Runtime
// (runtime.md §12, node-protocol.md).
func (r *Runtime) buildkitAvailable(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := r.buildkitClient(ctx)
	if err != nil {
		slog.Warn("BuildKit self-check failed; snapshot_build stays unsupported", "error", err)
		return false
	}
	defer c.Close() //nolint:errcheck // probe only
	if _, err = c.ListWorkers(ctx); err != nil {
		slog.Warn("BuildKit self-check failed; snapshot_build stays unsupported", "error", err)
		return false
	}
	return true
}
