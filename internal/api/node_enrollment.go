package api

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/store"
)

type nodeService struct {
	nodev1.UnimplementedNodeServer
	s *Server
}

// nodeServer builds the `node` listener (ADR-0014). It always uses TLS; the
// certificate is deployment configuration.
func (s *Server) nodeServer() (*grpc.Server, net.Listener, error) {
	if s.cfg.NodeTLSCert == "" || s.cfg.NodeTLSKey == "" {
		return nil, nil, fmt.Errorf("ORRERY_NODE_TLS_CERT and ORRERY_NODE_TLS_KEY are required")
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
