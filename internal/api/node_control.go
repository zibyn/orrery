package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/store"
)

// Control is one stream per Agent: Hello and HelloAck first, then observations
// up and commands down (docs/spec/node-protocol.md).
func (n *nodeService) Control(stream nodev1.Node_ControlServer) error {
	ctx := stream.Context()
	node, err := n.s.authenticateNode(ctx)
	if err != nil {
		return err
	}

	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "the first message must be Hello")
	}
	hello := first.GetHello()
	if hello == nil {
		return status.Error(codes.InvalidArgument, "the first message must be Hello")
	}
	if hello.NodeId != "" && hello.NodeId != node.ID.String() {
		return status.Error(codes.Unauthenticated, "the Node Token does not match node_id")
	}
	if accepted, _ := nodeproto.Compatible(hello.ProtoMajor, hello.ProtoMinor); !accepted {
		return incompatibleError("the Node Agent protocol version is not supported")
	}
	major, minor := int32(hello.ProtoMajor), int32(hello.ProtoMinor)
	if err := n.s.q.NodeHello(ctx, store.NodeHelloParams{
		ID:           node.ID,
		AgentVersion: &hello.AgentVersion,
		ProtoMajor:   &major,
		ProtoMinor:   &minor,
	}); err != nil {
		n.s.log.Error("record node hello", "node_id", node.ID, "error", err)
		return status.Error(codes.Internal, "record Hello")
	}
	if err := stream.Send(&nodev1.ControlMessage{Message: &nodev1.ControlMessage_HelloAck{
		HelloAck: &nodev1.HelloAck{
			Accepted: true,
			// The deployment credential for the platform registry (ADR-0014).
			RegistryCredential: n.s.cfg.RegistryCredential,
		},
	}}); err != nil {
		return err
	}

	// Commands are sent from this one goroutine; the handler below only
	// receives. A send error ends the goroutine and the stream soon after.
	// done is closed when an admin declares the Node lost or deletes it, so the
	// handler can return UNAUTHENTICATED even while Recv blocks.
	conn := &agentConn{nodeID: node.ID, send: make(chan *nodev1.ControlMessage, 64), done: make(chan struct{})}
	n.s.reconcile.hub.register(conn)
	defer n.s.reconcile.hub.unregister(conn)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-conn.done:
				return
			case msg := <-conn.send:
				if err := stream.Send(msg); err != nil {
					return
				}
			}
		}
	}()

	// Recv runs in its own goroutine so the handler can return from done while
	// it would otherwise block waiting for the Agent.
	recv := make(chan *nodev1.AgentMessage)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case recv <- msg:
			case <-ctx.Done():
				return
			case <-conn.done:
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-conn.done:
			return status.Error(codes.Unauthenticated, "the Node is no longer authorized")
		case err := <-recvErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case msg := <-recv:
			switch m := msg.Message.(type) {
			case *nodev1.AgentMessage_Heartbeat:
				if err := n.recordHeartbeat(ctx, node.ID, m.Heartbeat); err != nil {
					return status.Error(codes.Internal, "record Heartbeat")
				}
			case *nodev1.AgentMessage_Capabilities:
				if err := n.recordCapabilities(ctx, node.ID, m.Capabilities); err != nil {
					return status.Error(codes.Internal, "record Capabilities")
				}
			case *nodev1.AgentMessage_SandboxObserved:
				n.recordSandboxObserved(ctx, node.ID, m.SandboxObserved)
			case *nodev1.AgentMessage_WorkspaceObserved:
				n.recordWorkspaceObserved(ctx, node.ID, m.WorkspaceObserved)
			case *nodev1.AgentMessage_ExecObserved:
				n.recordExecObserved(ctx, node.ID, m.ExecObserved)
			case *nodev1.AgentMessage_BuildObserved:
				n.recordBuildObserved(ctx, node.ID, m.BuildObserved)
			case *nodev1.AgentMessage_CommandResult:
				// A Workspace publish is finalized from its CommandResult: success
				// verifies the objects, failure ends the version (node-protocol.md).
				n.handlePublishCommandResult(ctx, node, m.CommandResult)
				if m.CommandResult.Error != "" {
					n.s.log.Warn("command failed on Agent",
						"node_id", node.ID, "command_id", m.CommandResult.CommandId, "error", m.CommandResult.Error)
					// A failed command is not lost: the next sweep derives it again.
					n.s.reconcile.hub.failed(node.ID, m.CommandResult.CommandId)
					n.recordWorkspaceCommandFailure(ctx, m.CommandResult.CommandId)
					n.s.reconcile.notify()
				}
			default:
				return status.Error(codes.InvalidArgument, "unsupported AgentMessage")
			}
		}
	}
}

func (n *nodeService) recordHeartbeat(ctx context.Context, id uuid.UUID, heartbeat *nodev1.Heartbeat) error {
	var health *string
	if heartbeat.RuntimeHealth != "" {
		health = &heartbeat.RuntimeHealth
	}
	hardware, err := json.Marshal(map[string]any{
		"disk_free":      heartbeat.DiskFree,
		"disk_total":     heartbeat.DiskTotal,
		"disk_watermark": heartbeat.DiskWatermark,
		"gpu_health":     heartbeat.GpuHealth,
		"cpu_cores":      heartbeat.CpuCores,
		"memory_bytes":   heartbeat.MemoryBytes,
	})
	if err != nil {
		return err
	}
	// A failed inventory read must not cost the Node its heartbeat: the share
	// would be zero only until the next one.
	cards := 0
	if gpus, err := n.s.q.ListNodeGPUs(ctx, id); err == nil {
		cards = len(gpus)
	}
	cpuPerGPU, memoryPerGPU := perGPUShare(heartbeat, cards)
	err = n.s.q.NodeHeartbeat(ctx, store.NodeHeartbeatParams{
		ID:             id,
		RuntimeHealth:  health,
		Hardware:       hardware,
		ReservedCpu:    heartbeat.ReservedCpu,
		ReservedMemory: int64(heartbeat.ReservedMemory),
		CpuPerGpu:      cpuPerGPU,
		MemoryPerGpu:   memoryPerGPU,
	})
	if err != nil {
		n.s.log.Error("record heartbeat", "node_id", id, "error", err)
	}
	return err
}

// perGPUShare splits three quarters of the allocatable CPU and memory evenly
// across the Node's cards (defaults.md, ADR-0015). The remaining quarter is
// what GPU-less Sandboxes may use.
func perGPUShare(heartbeat *nodev1.Heartbeat, cards int) (cpu float64, memory int64) {
	if cards <= 0 {
		return 0, 0
	}
	allocCPU := heartbeat.CpuCores - heartbeat.ReservedCpu
	allocMemory := float64(heartbeat.MemoryBytes) - float64(heartbeat.ReservedMemory)
	if allocCPU < 0 {
		allocCPU = 0
	}
	if allocMemory < 0 {
		allocMemory = 0
	}
	return allocCPU * 0.75 / float64(cards), int64(allocMemory * 0.75 / float64(cards))
}

func (n *nodeService) recordCapabilities(ctx context.Context, id uuid.UUID, caps *nodev1.Capabilities) error {
	snapshot := nodeproto.CapabilityReport{RuntimeProvider: caps.RuntimeProvider}
	for _, entry := range caps.Entries {
		snapshot.Entries = append(snapshot.Entries, nodeproto.CapabilityEntry{
			Name:  entry.Name,
			Value: capabilityValue(entry.Value),
		})
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := n.s.q.WithTx(tx)
	if err := q.SetNodeCapabilities(ctx, store.SetNodeCapabilitiesParams{ID: id, Capabilities: raw}); err != nil {
		n.s.log.Error("store capabilities", "node_id", id, "error", err)
		return err
	}
	if err := q.DeleteNodeGPUs(ctx, id); err != nil {
		return err
	}
	for _, gpu := range caps.Gpus {
		if err := q.InsertNodeGPU(ctx, store.InsertNodeGPUParams{
			Uuid: gpu.Uuid, NodeID: id, Model: gpu.Model,
		}); err != nil {
			n.s.log.Error("store node GPU", "node_id", id, "error", err)
			return err
		}
	}
	return tx.Commit(ctx)
}

func capabilityValue(v nodev1.CapabilityValue) string {
	switch v {
	case nodev1.CapabilityValue_CAPABILITY_VALUE_ENFORCED:
		return nodeproto.ValueEnforced
	case nodev1.CapabilityValue_CAPABILITY_VALUE_BEST_EFFORT:
		return nodeproto.ValueBestEffort
	default:
		// Unspecified fails closed: an unstated capability is not a boundary.
		return nodeproto.ValueUnsupported
	}
}
