package store

import (
	"context"

	"github.com/google/uuid"
)

// ReleaseCompute drops a Sandbox's Resource Allocation and the GPU claims that
// belong to it, in that order: gpu_allocations restricts the Allocation's
// deletion (schema.md).
func (q *Queries) ReleaseCompute(ctx context.Context, sandboxID uuid.UUID) error {
	if err := q.DeleteGPUAllocations(ctx, sandboxID); err != nil {
		return err
	}
	return q.DeleteAllocation(ctx, sandboxID)
}
