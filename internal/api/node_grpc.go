package api

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/nodeproto"
	"github.com/zibyn/stars-sandbox/internal/store"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

type nodeService struct {
	nodev1.UnimplementedNodeServer
	s *Server
}

// nodeServer builds the `node` listener (ADR-0014). It always uses TLS; the
// certificate is deployment configuration.
func (s *Server) nodeServer() (*grpc.Server, net.Listener, error) {
	if s.cfg.NodeTLSCert == "" || s.cfg.NodeTLSKey == "" {
		return nil, nil, fmt.Errorf("STARS_NODE_TLS_CERT and STARS_NODE_TLS_KEY are required")
	}
	cert, err := tls.LoadX509KeyPair(s.cfg.NodeTLSCert, s.cfg.NodeTLSKey)
	if err != nil {
		return nil, nil, fmt.Errorf("load node TLS certificate: %w", err)
	}
	listener, err := net.Listen("tcp", s.cfg.NodeAddr)
	if err != nil {
		return nil, nil, fmt.Errorf("listen on node address: %w", err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})))
	nodev1.RegisterNodeServer(server, &nodeService{s: s})
	return server, listener, nil
}

// Redeem trades a single-use Enrollment Packet for a Node Token. Everything it
// needs to validate is in the request; TLS is the transport's only protection.
func (n *nodeService) Redeem(ctx context.Context, req *nodev1.RedeemRequest) (*nodev1.RedeemResponse, error) {
	if accepted, _ := nodeproto.Compatible(req.ProtoMajor, req.ProtoMinor); !accepted {
		return nil, incompatibleError("the Node Agent protocol version is not supported")
	}
	if !validDisplayName(req.Name) {
		return nil, status.Error(codes.InvalidArgument, "name must be non-empty, trimmed, and at most 255 characters")
	}
	parts, ok := ParseEnrollmentPacket(req.Packet)
	if !ok {
		return nil, errInvalidPacket
	}

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "begin transaction")
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := n.s.q.WithTx(tx)

	packet, err := q.GetEnrollmentPacket(ctx, parts.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errInvalidPacket
	}
	if err != nil {
		n.s.log.Error("read enrollment packet", "error", err)
		return nil, status.Error(codes.Internal, "read Enrollment Packet")
	}
	if packet.UsedAt.Valid || !packet.ExpiresAt.Time.After(time.Now()) ||
		subtle.ConstantTimeCompare(packet.SecretHash, HashSecret(parts.Secret)) != 1 {
		return nil, errInvalidPacket
	}
	// The UPDATE is the claim: two Agents redeeming at once cannot both win.
	if _, err := q.RedeemEnrollmentPacket(ctx, parts.ID); errors.Is(err, pgx.ErrNoRows) {
		return nil, errInvalidPacket
	} else if err != nil {
		n.s.log.Error("redeem enrollment packet", "error", err)
		return nil, status.Error(codes.Internal, "redeem Enrollment Packet")
	}

	raw, id, hash, err := NewNodeToken()
	if err != nil {
		return nil, status.Error(codes.Internal, "generate Node Token")
	}
	major, minor := int32(req.ProtoMajor), int32(req.ProtoMinor)
	node, err := q.CreateNode(ctx, store.CreateNodeParams{
		ID:           id,
		Name:         req.Name,
		TokenHash:    hash,
		AgentVersion: &req.AgentVersion,
		ProtoMajor:   &major,
		ProtoMinor:   &minor,
	})
	if err != nil {
		if uniqueViolation(err) == "nodes_name_key" {
			return nil, status.Error(codes.FailedPrecondition, "a Node with that name already exists")
		}
		n.s.log.Error("create node", "error", err)
		return nil, status.Error(codes.Internal, "create Node")
	}
	if err := tx.Commit(ctx); err != nil {
		n.s.log.Error("commit node enrollment", "error", err)
		return nil, status.Error(codes.Internal, "commit enrollment")
	}

	n.s.auditAs(ctx, nil, nil, "node.enroll", "node", node.ID.String(), map[string]any{
		"name":          node.Name,
		"agent_version": req.AgentVersion,
		"proto_major":   req.ProtoMajor,
		"proto_minor":   req.ProtoMinor,
	})
	return &nodev1.RedeemResponse{NodeId: node.ID.String(), NodeToken: raw}, nil
}

var errInvalidPacket = status.Error(codes.Unauthenticated, "invalid, used, or expired Enrollment Packet")

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
		HelloAck: &nodev1.HelloAck{Accepted: true},
	}}); err != nil {
		return err
	}

	// Commands are sent from this one goroutine; the handler below only
	// receives. A send error ends the goroutine and the stream soon after.
	conn := &agentConn{nodeID: node.ID, send: make(chan *nodev1.ControlMessage, 64)}
	n.s.reconcile.hub.register(conn)
	defer n.s.reconcile.hub.unregister(conn)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-conn.send:
				if err := stream.Send(msg); err != nil {
					return
				}
			}
		}
	}()

	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
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
		case *nodev1.AgentMessage_CommandResult:
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
	snapshot := nodeproto.CapabilityReport{Runtime: caps.Runtime}
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

func (s *Server) authenticateNode(ctx context.Context) (store.Node, error) {
	token := bearerFromMetadata(ctx)
	parts, ok := ParseNodeToken(token)
	if !ok {
		return store.Node{}, status.Error(codes.Unauthenticated, "a Node Token is required")
	}
	node, err := s.q.GetNode(ctx, parts.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Node{}, status.Error(codes.Unauthenticated, "unknown Node Token")
	}
	if err != nil {
		s.log.Error("look up node", "error", err)
		return store.Node{}, status.Error(codes.Internal, "look up Node")
	}
	if subtle.ConstantTimeCompare(node.TokenHash, HashSecret(parts.Secret)) != 1 {
		return store.Node{}, status.Error(codes.Unauthenticated, "unknown Node Token")
	}
	return node, nil
}

func bearerFromMetadata(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	for _, value := range md.Get("authorization") {
		scheme, rest, ok := strings.Cut(value, " ")
		if ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// incompatibleError tells the Agent to keep its identity but retry slowly
// (docs/spec/node-protocol.md "错误映射", ADR-0014).
func incompatibleError(detail string) error {
	st := status.New(codes.FailedPrecondition, detail)
	withDetails, err := st.WithDetails(&errdetails.ErrorInfo{
		Reason:   "incompatible",
		Domain:   "stars-sandbox",
		Metadata: map[string]string{"detail": detail},
	})
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}

// recordSandboxObserved applies what the Agent saw with the generation fence:
// an observation from an older generation is dropped (ADR-0015).
func (n *nodeService) recordSandboxObserved(ctx context.Context, nodeID uuid.UUID, obs *nodev1.SandboxObserved) {
	id, err := uuid.Parse(obs.SandboxId)
	if err != nil {
		return
	}
	// Only the Node the Sandbox is bound to may report on it (ADR-0010).
	sb, err := n.s.q.GetSandbox(ctx, id)
	if err != nil || sb.NodeID == nil || *sb.NodeID != nodeID {
		return
	}
	switch obs.Phase {
	case "running":
		rows, err := n.s.q.MarkSandboxReady(ctx, store.MarkSandboxReadyParams{
			ID: id, Generation: obs.Generation,
		})
		if err != nil {
			n.s.log.Warn("record SandboxObserved running", "sandbox_id", id, "error", err)
		}
		if rows > 0 {
			n.s.reconcile.notify()
		}
	case "create_failed":
		cause := obs.Cause
		if cause == "" {
			cause = "runtime_create_failed"
		}
		rows, err := n.s.q.MarkSandboxFailed(ctx, store.MarkSandboxFailedParams{
			ID: id, Generation: obs.Generation, Cause: &cause,
		})
		if err != nil {
			n.s.log.Warn("record SandboxObserved create_failed", "sandbox_id", id, "error", err)
			return
		}
		if rows > 0 {
			n.releaseCompute(ctx, id)
			n.s.reconcile.notify()
		}
	case "gone":
		n.recordSandboxGone(ctx, id, obs.Generation)
	}
}

func (n *nodeService) recordSandboxGone(ctx context.Context, id uuid.UUID, generation int64) {
	// The runtime can no longer produce output; an Exec still marked active is
	// lost with it (api.md §4).
	if _, err := n.s.q.FinishSandboxExecs(ctx, store.FinishSandboxExecsParams{
		SandboxID: id, Cause: strPtr("runtime_lost"), Generation: generation,
	}); err != nil {
		n.s.log.Warn("finish lost Execs", "sandbox_id", id, "error", err)
	}
	stopped, err := n.s.q.MarkSandboxStopped(ctx, store.MarkSandboxStoppedParams{
		ID: id, Generation: generation,
	})
	if err != nil {
		n.s.log.Warn("record SandboxObserved gone", "sandbox_id", id, "error", err)
		return
	}
	lost, err := n.s.q.MarkSandboxRuntimeLost(ctx, store.MarkSandboxRuntimeLostParams{
		ID: id, Generation: generation, Cause: strPtr("runtime_lost"),
	})
	if err != nil {
		n.s.log.Warn("record runtime lost", "sandbox_id", id, "error", err)
		return
	}
	if stopped > 0 || lost > 0 {
		n.releaseCompute(ctx, id)
		n.s.reconcile.notify()
		return
	}
	// A deleted Sandbox waits for its runtime to be gone before the Workspace
	// can be removed.
	sb, err := n.s.q.GetSandbox(ctx, id)
	if err != nil || sb.DesiredState != "deleted" {
		return
	}
	tx, err := n.s.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := n.s.q.WithTx(tx)
	if err := releaseCompute(ctx, q, id); err != nil {
		n.s.log.Warn("release compute", "sandbox_id", id, "error", err)
		return
	}
	ws, wsErr := q.GetSandboxWorkspace(ctx, id)
	switch {
	case wsErr == nil && ws.Kind == "ephemeral":
		if _, err := q.SetWorkspaceDeleting(ctx, ws.ID); err != nil {
			n.s.log.Warn("mark workspace deleting", "workspace_id", ws.ID, "error", err)
			return
		}
	case wsErr != nil && !errors.Is(wsErr, pgx.ErrNoRows):
		// A transient read error must not orphan an implicit Workspace; the
		// next sweep sends the stop again.
		n.s.log.Warn("look up Workspace for teardown", "sandbox_id", id, "error", wsErr)
		return
	default:
		if _, err := q.MarkSandboxDeleted(ctx, id); err != nil {
			n.s.log.Warn("mark Sandbox deleted", "sandbox_id", id, "error", err)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		n.s.log.Warn("commit Sandbox teardown", "sandbox_id", id, "error", err)
		return
	}
	n.s.reconcile.notify()
}

// recordExecObserved stamps the execve time and its display-only deadline.
func (n *nodeService) recordExecObserved(ctx context.Context, nodeID uuid.UUID, obs *nodev1.ExecObserved) {
	id, err := uuid.Parse(obs.ExecId)
	if err != nil {
		return
	}
	row, err := n.s.q.GetExecWithNode(ctx, id)
	if err != nil || row.SandboxNodeID == nil || *row.SandboxNodeID != nodeID {
		return
	}
	started, err := time.Parse(time.RFC3339Nano, obs.StartedAt)
	if err != nil {
		n.s.log.Warn("parse Exec started_at", "exec_id", id, "error", err)
		return
	}
	if err := n.s.q.SetExecStarted(ctx, store.SetExecStartedParams{
		ID: id, StartedAt: pgtype.Timestamptz{Time: started, Valid: true},
	}); err != nil {
		n.s.log.Warn("record Exec started", "exec_id", id, "error", err)
	}
}

// ExecOutput is one stream per Exec: frames in, acknowledgements out. The
// Agent resumes from stored_seq, so a lost batch is replayed, never duplicated.
func (n *nodeService) ExecOutput(stream nodev1.Node_ExecOutputServer) error {
	ctx := stream.Context()
	node, err := n.s.authenticateNode(ctx)
	if err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Error(codes.InvalidArgument, "the first message must name the Exec")
	}
	execID, err := uuid.Parse(first.GetExecId())
	if err != nil {
		return status.Error(codes.InvalidArgument, "exec_id must be a UUID")
	}
	row, err := n.s.q.GetExecWithNode(ctx, execID)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.NotFound, "unknown Exec")
	}
	if err != nil {
		n.s.log.Error("look up Exec for output", "exec_id", execID, "error", err)
		return status.Error(codes.Internal, "look up Exec")
	}
	if row.SandboxNodeID == nil || *row.SandboxNodeID != node.ID {
		return status.Error(codes.PermissionDenied, "the Exec does not belong to this Node")
	}
	stored := row.Exec.StoredSeq
	if err := stream.Send(&nodev1.OutputAck{ExecId: execID.String(), StoredSeq: stored}); err != nil {
		return err
	}

	for {
		batch, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if batch.GetExecId() != execID.String() {
			return status.Error(codes.InvalidArgument, "exec_id changed mid-stream")
		}
		frames := make([]logFrame, 0, len(batch.Frames))
		var exit *nodev1.OutputFrame
		truncated := false
		for _, f := range batch.Frames {
			if f.Stream == "exit" {
				exit = f
				continue
			}
			switch f.Stream {
			case "stdout", "stderr", "truncated":
			default:
				return status.Errorf(codes.InvalidArgument, "unknown output stream %q", f.Stream)
			}
			if f.Seq < stored {
				continue // already stored; the ack was lost
			}
			if f.Seq != stored {
				return status.Errorf(codes.InvalidArgument, "out-of-order frame: got %d, want %d", f.Seq, stored)
			}
			if f.Stream == "truncated" {
				truncated = true
			}
			frames = append(frames, logFrameFromProto(f.Seq, f.Stream, f.Data))
			stored = f.Seq + supervisor.HeaderSize + int64(len(f.Data))
		}
		if len(frames) > 0 {
			raw, err := json.Marshal(frames)
			if err != nil {
				return status.Error(codes.Internal, "encode output")
			}
			if err := n.s.q.AppendExecChunk(ctx, store.AppendExecChunkParams{
				ExecID: execID, FirstSeq: frames[0].Seq, LastSeq: stored, Frames: raw,
			}); err != nil {
				n.s.log.Error("append Exec output", "exec_id", execID, "error", err)
				return status.Error(codes.Internal, "store output")
			}
			if err := n.s.q.AdvanceExecStoredSeq(ctx, store.AdvanceExecStoredSeqParams{
				ID: execID, StoredSeq: stored, LogTruncated: truncated,
			}); err != nil {
				n.s.log.Error("advance Exec stored_seq", "exec_id", execID, "error", err)
				return status.Error(codes.Internal, "store output")
			}
		}
		if exit != nil {
			cause := exit.Cause
			if cause == "" {
				cause = "exited"
			}
			if _, err := n.s.q.FinishExec(ctx, store.FinishExecParams{
				ID: execID, Cause: &cause, ExitCode: exit.ExitCode, Signal: exit.Signal,
			}); err != nil {
				n.s.log.Error("finish Exec", "exec_id", execID, "error", err)
				return status.Error(codes.Internal, "store result")
			}
			if err := n.s.q.AdvanceExecStoredSeq(ctx, store.AdvanceExecStoredSeqParams{
				ID: execID, StoredSeq: exit.Seq, LogTruncated: truncated,
			}); err != nil {
				n.s.log.Error("advance Exec stored_seq", "exec_id", execID, "error", err)
				return status.Error(codes.Internal, "store result")
			}
			n.s.reconcile.notify()
			return stream.Send(&nodev1.OutputAck{ExecId: execID.String(), StoredSeq: exit.Seq})
		}
		if err := stream.Send(&nodev1.OutputAck{ExecId: execID.String(), StoredSeq: stored}); err != nil {
			return err
		}
	}
}

// releaseCompute drops the Allocation and the Attachment, which exist only
// while a runtime does. Both are attempted so one failure does not hide the
// other.
func releaseCompute(ctx context.Context, q *store.Queries, id uuid.UUID) error {
	var errs []error
	if err := q.ReleaseCompute(ctx, id); err != nil {
		errs = append(errs, fmt.Errorf("release allocation: %w", err))
	}
	if err := q.DeleteAttachment(ctx, id); err != nil {
		errs = append(errs, fmt.Errorf("release attachment: %w", err))
	}
	return errors.Join(errs...)
}

func (n *nodeService) releaseCompute(ctx context.Context, id uuid.UUID) {
	if err := releaseCompute(ctx, n.s.q, id); err != nil {
		n.s.log.Warn("release compute", "sandbox_id", id, "error", err)
	}
}

func (n *nodeService) recordWorkspaceObserved(ctx context.Context, nodeID uuid.UUID, obs *nodev1.WorkspaceObserved) {
	id, err := uuid.Parse(obs.WorkspaceId)
	if err != nil {
		return
	}
	ws, err := n.s.q.GetWorkspace(ctx, id)
	if err != nil || ws.NodeID == nil || *ws.NodeID != nodeID {
		return
	}
	switch obs.Phase {
	case "ready":
		rows, err := n.s.q.MarkWorkspaceReady(ctx, store.MarkWorkspaceReadyParams{
			ID: id, Generation: obs.Generation,
		})
		if err != nil {
			n.s.log.Warn("record WorkspaceObserved ready", "workspace_id", id, "error", err)
			return
		}
		if rows > 0 {
			if err := n.s.q.SetWorkspaceConditions(ctx, store.SetWorkspaceConditionsParams{ID: id}); err != nil {
				n.s.log.Warn("clear Workspace conditions", "workspace_id", id, "error", err)
			}
			n.s.reconcile.notify()
		}
	case "gone":
		rows, err := n.s.q.MarkWorkspaceDeleted(ctx, store.MarkWorkspaceDeletedParams{
			ID: id, Generation: obs.Generation,
		})
		if err != nil {
			n.s.log.Warn("record WorkspaceObserved gone", "workspace_id", id, "error", err)
			return
		}
		if rows > 0 {
			if ws, err := n.s.q.GetWorkspace(ctx, id); err == nil && ws.SandboxID != nil {
				if _, err := n.s.q.MarkSandboxDeleted(ctx, *ws.SandboxID); err != nil {
					n.s.log.Warn("mark sandbox deleted", "sandbox_id", *ws.SandboxID, "error", err)
				}
			}
			n.s.reconcile.notify()
		}
	}
}

// recordWorkspaceCommandFailure advances a Workspace's provisioning retry
// state. Backoff and the stalled condition follow defaults.md; once the
// retries are used up the Workspace ends failed (ADR-0015).
// ponytail: only a reported CommandResult counts as a failed attempt. An Agent
// that accepts the command and never reports leaves the Workspace
// provisioning; add a per-attempt timeout if a silently hung Agent matters.
func (n *nodeService) recordWorkspaceCommandFailure(ctx context.Context, commandID string) {
	const prefix = "provision-workspace:"
	if !strings.HasPrefix(commandID, prefix) {
		return
	}
	idPart, _, ok := strings.Cut(strings.TrimPrefix(commandID, prefix), ":")
	if !ok {
		return
	}
	id, err := uuid.Parse(idPart)
	if err != nil {
		return
	}
	ws, err := n.s.q.GetWorkspace(ctx, id)
	if err != nil || ws.State != "provisioning" {
		return
	}
	conditions := parseWorkspaceConditions(ws.Conditions)
	conditions.Attempts++
	if conditions.Attempts > len(workspaceProvisionBackoff) {
		cause := "provisioning_failed"
		if _, err := n.s.q.MarkWorkspaceFailed(ctx, store.MarkWorkspaceFailedParams{
			ID: id, Generation: ws.Generation, Cause: &cause,
		}); err != nil {
			n.s.log.Warn("record Workspace failure", "workspace_id", id, "error", err)
		}
		return
	}
	next := time.Now().Add(workspaceProvisionBackoff[conditions.Attempts-1])
	conditions.NextAttemptAt = &next
	if conditions.Attempts >= workspaceStalledAfter {
		conditions.ProvisioningStalled = true
	}
	if err := n.s.q.SetWorkspaceConditions(ctx, store.SetWorkspaceConditionsParams{
		ID: id, Conditions: encodeWorkspaceConditions(conditions),
	}); err != nil {
		n.s.log.Warn("record Workspace retry", "workspace_id", id, "error", err)
	}
}

func strPtr(s string) *string { return &s }
