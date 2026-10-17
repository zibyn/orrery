package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/zibyn/stars-sandbox/internal/store"
)

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
