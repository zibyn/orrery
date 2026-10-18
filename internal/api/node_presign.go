package api

import (
	"context"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// Presign issues object store URLs to an authenticated Node
// (docs/spec/node-protocol.md "Presign"). It signs nothing the Node is not
// entitled to, and answers PermissionDenied for the whole batch otherwise.
func (n *nodeService) Presign(ctx context.Context, req *nodev1.PresignRequest) (*nodev1.PresignResponse, error) {
	node, err := n.s.authenticateNode(ctx)
	if err != nil {
		return nil, err
	}
	method := req.GetMethod()
	if method != "GET" && method != "PUT" {
		return nil, status.Error(codes.InvalidArgument, `method must be "GET" or "PUT"`)
	}
	if (len(req.GetSha256()) == 0) == (len(req.GetManifests()) == 0) {
		return nil, status.Error(codes.InvalidArgument, "exactly one of sha256 or manifests is required")
	}
	if method == "PUT" && len(req.GetManifests()) > 0 {
		return nil, status.Error(codes.InvalidArgument, "manifests can only be signed for GET")
	}
	if n.s.objects == nil {
		return nil, status.Error(codes.Unavailable, "object storage is not configured")
	}

	// Authorize the whole batch before signing anything. The owner comes from
	// the reference row: CAS keys are owner-scoped (ADR-0021).
	owners := make([]uuid.UUID, 0, len(req.GetSha256()))
	for _, digest := range req.GetSha256() {
		var owner uuid.UUID
		var ok bool
		if method == "PUT" {
			owner, ok, err = n.s.authorizePublishObject(ctx, node, digest)
		} else {
			owner, ok, err = n.s.authorizeSandboxObject(ctx, node, digest)
		}
		if err != nil {
			n.s.log.Error("authorize presign object", "node_id", node.ID, "error", err)
			return nil, status.Error(codes.Internal, "authorize object")
		}
		if !ok {
			return nil, status.Error(codes.PermissionDenied, "object is not authorized for this Node")
		}
		owners = append(owners, owner)
	}
	manifestOwners := make([]uuid.UUID, 0, len(req.GetManifests()))
	for _, address := range req.GetManifests() {
		owner, ok, err := n.s.authorizeSandboxManifest(ctx, node, address)
		if err != nil {
			n.s.log.Error("authorize presign manifest", "node_id", node.ID, "error", err)
			return nil, status.Error(codes.Internal, "authorize manifest")
		}
		if !ok {
			return nil, status.Error(codes.PermissionDenied, "manifest is not authorized for this Node")
		}
		manifestOwners = append(manifestOwners, owner)
	}

	resp := &nodev1.PresignResponse{Urls: make([]string, 0, len(req.GetSha256())+len(req.GetManifests()))}
	for i, digest := range req.GetSha256() {
		var url string
		if method == "PUT" {
			url, err = n.s.objects.PresignPutURL(ctx, owners[i].String(), digest)
		} else {
			url, err = n.s.objects.PresignGetURL(ctx, owners[i].String(), digest)
		}
		if err != nil {
			n.s.log.Error("presign object", "node_id", node.ID, "error", err)
			return nil, status.Error(codes.Internal, "presign object")
		}
		resp.Urls = append(resp.Urls, url)
	}
	for i, address := range req.GetManifests() {
		url, err := n.s.objects.PresignManifestGetURL(ctx, manifestOwners[i].String(), address)
		if err != nil {
			n.s.log.Error("presign manifest", "node_id", node.ID, "error", err)
			return nil, status.Error(codes.Internal, "presign manifest")
		}
		resp.Urls = append(resp.Urls, url)
	}
	return resp, nil
}

// The authorization queries below are the seam that later tickets fill in:
// only objects of a Sandbox allocated to this Node (and that Sandbox's
// ResourceVersions) may be signed for GET, and only objects missing from a
// publish running on this Node may be signed for PUT. Each returns the CAS
// owner whose key the URL addresses (ADR-0021: keys are owner-scoped, and a
// Node is not a User). Until the reference tables exist nothing is authorized.

// authorizeSandboxObject reports whether sha256 is stored in a ResourceVersion
// mounted by a Sandbox (including a build row) allocated to this Node.
//
// TODO(#86): implement against sandbox_resources and
// resource_version_objects.
func (s *Server) authorizeSandboxObject(ctx context.Context, node store.Node, sha256 string) (uuid.UUID, bool, error) {
	_, _, _ = ctx, node, sha256
	return uuid.Nil, false, nil
}

// authorizePublishObject reports whether sha256 is missing from an upload for
// a publish in progress on this Node.
//
// TODO(#87): implement against uploads and resource_versions.
func (s *Server) authorizePublishObject(ctx context.Context, node store.Node, sha256 string) (uuid.UUID, bool, error) {
	_, _, _ = ctx, node, sha256
	return uuid.Nil, false, nil
}

// authorizeSandboxManifest reports whether address is the manifest of a
// ResourceVersion mounted by a Sandbox allocated to this Node.
//
// TODO(#86): implement against sandbox_resources and resource_versions.
func (s *Server) authorizeSandboxManifest(ctx context.Context, node store.Node, address string) (uuid.UUID, bool, error) {
	_, _, _ = ctx, node, address
	return uuid.Nil, false, nil
}
