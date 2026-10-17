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
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/nodeproto"
	"github.com/zibyn/stars-sandbox/internal/store"
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

// Control is one stream per Agent: Hello, Heartbeat and Capabilities for now
// (docs/spec/node-protocol.md). Commands and status arrive with later tickets.
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
		"disk_free":  heartbeat.DiskFree,
		"gpu_health": heartbeat.GpuHealth,
	})
	if err != nil {
		return err
	}
	err = n.s.q.NodeHeartbeat(ctx, store.NodeHeartbeatParams{
		ID:            id,
		RuntimeHealth: health,
		Hardware:      hardware,
	})
	if err != nil {
		n.s.log.Error("record heartbeat", "node_id", id, "error", err)
	}
	return err
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
