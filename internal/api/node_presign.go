package api

import (
	"context"
	"errors"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/objectstore"
	"github.com/zibyn/stars-sandbox/internal/store"
)

// Presign issues object store URLs to an authenticated Node
// (docs/spec/node-protocol.md "Presign"). It signs nothing the Node is not
// entitled to, and answers InvalidArgument for the whole batch otherwise
// (node-protocol.md: never NotFound, which would make the Agent clear its
// identity).
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
			return nil, status.Error(codes.InvalidArgument, "object is not authorized for this Node")
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
			return nil, status.Error(codes.InvalidArgument, "manifest is not authorized for this Node")
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

// The authorization queries below are the seam between the Node and the
// reference tables: only objects of a Sandbox allocated to this Node (and that
// Sandbox's ResourceVersions) may be signed for GET, and only objects missing
// from a publish running on this Node may be signed for PUT. Each returns the
// CAS owner whose key the URL addresses (ADR-0021: keys are owner-scoped, and
// a Node is not a User).

// authorizeSandboxObject reports whether sha256 is stored in a ResourceVersion
// mounted by a Sandbox (including a build row) allocated to this Node.
//
// If every owner-scoped copy is missing from the object store, the versions
// that reference it are marked unavailable and the stale cas_objects rows are
// deleted, so a later upload re-issues PUTs (schema.md, api.md §3). The
// signature is refused either way: the Agent reports resource_fetch_failed
// after its retries.
func (s *Server) authorizeSandboxObject(ctx context.Context, node store.Node, sha256 string) (uuid.UUID, bool, error) {
	refs, err := s.q.ListNodeSandboxObjectRefs(ctx, store.ListNodeSandboxObjectRefsParams{
		NodeID: &node.ID, Sha256: sha256,
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	if len(refs) == 0 {
		return uuid.Nil, false, nil
	}
	// The same bytes may legitimately belong to several owners on one Node.
	// Any existing owner-scoped copy serves the content; only when none
	// exists is the version actually unavailable.
	owners := make([]uuid.UUID, 0, len(refs))
	seen := make(map[uuid.UUID]struct{}, len(refs))
	for _, ref := range refs {
		if _, exists := seen[ref.OwnerID]; exists {
			continue
		}
		seen[ref.OwnerID] = struct{}{}
		owners = append(owners, ref.OwnerID)
	}
	sort.Slice(owners, func(i, j int) bool { return owners[i].String() < owners[j].String() })
	for _, owner := range owners {
		stored, err := s.objects.HeadSHA256(ctx, owner.String(), sha256)
		if err == nil && stored == sha256 {
			return owner, true, nil
		}
		if err != nil && !errors.Is(err, objectstore.ErrNotFound) {
			return uuid.Nil, false, err
		}
	}
	if err := s.markObjectsMissing(ctx, sha256, refs, owners); err != nil {
		return uuid.Nil, false, err
	}
	return uuid.Nil, false, nil
}

// markObjectsMissing records the object-store 404: every referencing version
// on this Node becomes unavailable and the CAS rows that named the objects go
// away (schema.md).
func (s *Server) markObjectsMissing(ctx context.Context, sha256 string, refs []store.ListNodeSandboxObjectRefsRow, owners []uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit
	q := s.q.WithTx(tx)
	for _, ref := range refs {
		if _, err := q.MarkResourceVersionUnavailable(ctx, ref.VersionID); err != nil {
			return err
		}
	}
	for _, owner := range owners {
		if err := q.DeleteCASObject(ctx, store.DeleteCASObjectParams{
			OwnerID: owner, Sha256: sha256,
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// authorizePublishObject reports whether sha256 is one of the objects of a
// pending Workspace publish assigned to this Node and is still missing from
// the store (node-protocol.md "Presign"). It returns the Resource owner whose
// CAS key the URL addresses (ADR-0021: keys are owner-scoped).
func (s *Server) authorizePublishObject(ctx context.Context, node store.Node, sha256 string) (uuid.UUID, bool, error) {
	if s.objects == nil {
		return uuid.Nil, false, nil
	}
	nodeID := node.ID
	owner, err := s.q.FindPublishObjectOwner(ctx, store.FindPublishObjectOwnerParams{
		NodeID: &nodeID, Sha256: sha256,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	// The Agent receives the missing set from SubmitManifest, but verify it:
	// an object that is already stored and correct needs no PUT.
	stored, err := s.objects.HeadSHA256(ctx, owner.String(), sha256)
	if err == nil && stored == sha256 {
		return uuid.Nil, false, nil
	}
	if err != nil && !errors.Is(err, objectstore.ErrNotFound) {
		return uuid.Nil, false, err
	}
	return owner, true, nil
}

// authorizeSandboxManifest reports whether address is the manifest of a
// ResourceVersion mounted by a Sandbox allocated to this Node.
func (s *Server) authorizeSandboxManifest(ctx context.Context, node store.Node, address string) (uuid.UUID, bool, error) {
	owner, err := s.q.ListNodeSandboxManifestOwner(ctx, store.ListNodeSandboxManifestOwnerParams{
		NodeID: &node.ID, Address: &address,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return owner, true, nil
}
