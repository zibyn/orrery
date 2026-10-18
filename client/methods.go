package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// pageQuery builds the shared ?limit=&cursor= pair (api.md §1).
func pageQuery(limit int, cursor string) url.Values {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	return q
}

// Sessions, Users and tokens.

// CreateSession logs in with a username and password and returns a new token.
func (c *Client) CreateSession(ctx context.Context, req SessionRequest) (*CreatedToken, error) {
	var out CreatedToken
	err := c.do(ctx, http.MethodPost, "/api/v1/sessions", nil, req, &out, "")
	return &out, err
}

// Me returns the authenticated User.
func (c *Client) Me(ctx context.Context) (*User, error) {
	var out User
	err := c.do(ctx, http.MethodGet, "/api/v1/users/me", nil, nil, &out, "")
	return &out, err
}

// MyQuota returns the authenticated User's quota and usage.
func (c *Client) MyQuota(ctx context.Context) (*UserQuota, error) {
	var out UserQuota
	err := c.do(ctx, http.MethodGet, "/api/v1/users/me/quota", nil, nil, &out, "")
	return &out, err
}

// CreateUser creates a User (admin only).
func (c *Client) CreateUser(ctx context.Context, req CreateUserRequest, idempotencyKey string) (*User, error) {
	var out User
	err := c.do(ctx, http.MethodPost, "/api/v1/users", nil, req, &out, idempotencyKey)
	return &out, err
}

// ListUsers returns one page of Users (admin only).
func (c *Client) ListUsers(ctx context.Context, limit int, cursor string) (*UserList, error) {
	var out UserList
	err := c.do(ctx, http.MethodGet, "/api/v1/users", pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// GetUser returns one User (admin only).
func (c *Client) GetUser(ctx context.Context, id string) (*User, error) {
	var out User
	err := c.do(ctx, http.MethodGet, "/api/v1/users/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// UpdateUser patches one User (admin only).
func (c *Client) UpdateUser(ctx context.Context, id string, req UpdateUserRequest) (*User, error) {
	var out User
	err := c.do(ctx, http.MethodPatch, "/api/v1/users/"+url.PathEscape(id), nil, req, &out, "")
	return &out, err
}

// DeleteUser deletes one User (admin only).
func (c *Client) DeleteUser(ctx context.Context, id string) (*User, error) {
	var out User
	err := c.do(ctx, http.MethodDelete, "/api/v1/users/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// SetUserQuota sets a User's GPU and Sandbox limits (admin only).
func (c *Client) SetUserQuota(ctx context.Context, id string, gpus, sandboxes int32) (*User, error) {
	var out User
	err := c.do(ctx, http.MethodPut, "/api/v1/users/"+url.PathEscape(id)+"/quota", nil,
		Quota{GPUs: gpus, Sandboxes: sandboxes}, &out, "")
	return &out, err
}

// CreateToken creates an API Token for the authenticated User.
func (c *Client) CreateToken(ctx context.Context, req TokenRequest, idempotencyKey string) (*CreatedToken, error) {
	var out CreatedToken
	err := c.do(ctx, http.MethodPost, "/api/v1/users/me/tokens", nil, req, &out, idempotencyKey)
	return &out, err
}

// ListTokens returns one page of the authenticated User's API Tokens.
func (c *Client) ListTokens(ctx context.Context, limit int, cursor string) (*TokenList, error) {
	var out TokenList
	err := c.do(ctx, http.MethodGet, "/api/v1/users/me/tokens", pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// RevokeToken revokes one of the authenticated User's API Tokens.
func (c *Client) RevokeToken(ctx context.Context, id string) (*Token, error) {
	var out Token
	err := c.do(ctx, http.MethodDelete, "/api/v1/users/me/tokens/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// Secrets.

// PutSecret creates or overwrites a Secret.
func (c *Client) PutSecret(ctx context.Context, name, value string) (*Secret, error) {
	var out Secret
	err := c.do(ctx, http.MethodPut, "/api/v1/secrets/"+url.PathEscape(name), nil,
		map[string]string{"value": value}, &out, "")
	return &out, err
}

// ListSecrets returns one page of Secrets, without their values.
func (c *Client) ListSecrets(ctx context.Context, limit int, cursor string) (*SecretList, error) {
	var out SecretList
	err := c.do(ctx, http.MethodGet, "/api/v1/secrets", pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// DeleteSecret deletes one Secret.
func (c *Client) DeleteSecret(ctx context.Context, name string) (*Secret, error) {
	var out Secret
	err := c.do(ctx, http.MethodDelete, "/api/v1/secrets/"+url.PathEscape(name), nil, nil, &out, "")
	return &out, err
}

// Git.

// GitCredential returns a short-lived Git credential for a host or provider.
func (c *Client) GitCredential(ctx context.Context, req GitCredentialRequest) (*GitCredential, error) {
	var out GitCredential
	err := c.do(ctx, http.MethodPost, "/api/v1/git/credential", nil, req, &out, "")
	return &out, err
}

// ListGitLinks returns the authenticated User's linked Git accounts.
func (c *Client) ListGitLinks(ctx context.Context) (*GitLinkList, error) {
	var out GitLinkList
	err := c.do(ctx, http.MethodGet, "/api/v1/users/me/git-links", nil, nil, &out, "")
	return &out, err
}

// DeleteGitLink unlinks one Git provider account.
func (c *Client) DeleteGitLink(ctx context.Context, providerID string) (*GitLink, error) {
	var out GitLink
	err := c.do(ctx, http.MethodDelete, "/api/v1/users/me/git-links/"+url.PathEscape(providerID), nil, nil, &out, "")
	return &out, err
}

// Audit.

// ListAudit returns one filtered page of audit entries.
func (c *Client) ListAudit(ctx context.Context, params AuditParams) (*AuditList, error) {
	q := pageQuery(params.Limit, params.Cursor)
	for name, value := range map[string]string{
		"actor": params.Actor, "action": params.Action, "target": params.Target,
		"target_type": params.TargetType, "from": params.From, "to": params.To,
	} {
		if value != "" {
			q.Set(name, value)
		}
	}
	var out AuditList
	err := c.do(ctx, http.MethodGet, "/api/v1/audit", q, nil, &out, "")
	return &out, err
}

// Nodes and capacity.

// CreateEnrollmentPacket creates a packet for enrolling a Node (admin only).
func (c *Client) CreateEnrollmentPacket(ctx context.Context) (*EnrollmentPacket, error) {
	var out EnrollmentPacket
	err := c.do(ctx, http.MethodPost, "/api/v1/enrollment-packets", nil, nil, &out, "")
	return &out, err
}

// ListNodes returns one page of Nodes.
func (c *Client) ListNodes(ctx context.Context, limit int, cursor string) (*NodeList, error) {
	var out NodeList
	err := c.do(ctx, http.MethodGet, "/api/v1/nodes", pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// GetNode returns one Node.
func (c *Client) GetNode(ctx context.Context, id string) (*Node, error) {
	var out Node
	err := c.do(ctx, http.MethodGet, "/api/v1/nodes/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// DeleteNode deletes one Node (admin only).
func (c *Client) DeleteNode(ctx context.Context, id string) (*Node, error) {
	var out Node
	err := c.do(ctx, http.MethodDelete, "/api/v1/nodes/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// PauseNode stops new Sandboxes from scheduling on a Node (admin only).
func (c *Client) PauseNode(ctx context.Context, id string) (*Node, error) {
	return c.nodeAction(ctx, id, "pause")
}

// ResumeNode lets a paused Node schedule Sandboxes again (admin only).
func (c *Client) ResumeNode(ctx context.Context, id string) (*Node, error) {
	return c.nodeAction(ctx, id, "resume")
}

// DeclareNodeLost marks a Node lost and its Sandboxes for recovery (admin only).
func (c *Client) DeclareNodeLost(ctx context.Context, id string) (*Node, error) {
	return c.nodeAction(ctx, id, "declare-lost")
}

func (c *Client) nodeAction(ctx context.Context, id, action string) (*Node, error) {
	var out Node
	err := c.do(ctx, http.MethodPost, "/api/v1/nodes/"+url.PathEscape(id)+"/"+action, nil, nil, &out, "")
	return &out, err
}

// Capacity returns GPU capacity per Node.
func (c *Client) Capacity(ctx context.Context) (*CapacityList, error) {
	var out CapacityList
	err := c.do(ctx, http.MethodGet, "/api/v1/capacity", nil, nil, &out, "")
	return &out, err
}

// Resources.

// CreateResource creates a Resource by name.
func (c *Client) CreateResource(ctx context.Context, name string) (*Resource, error) {
	var out Resource
	err := c.do(ctx, http.MethodPost, "/api/v1/resources", nil, map[string]string{"name": name}, &out, "")
	return &out, err
}

// ListResources returns one page of Resources.
func (c *Client) ListResources(ctx context.Context, limit int, cursor string) (*ResourceList, error) {
	var out ResourceList
	err := c.do(ctx, http.MethodGet, "/api/v1/resources", pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// GetResource returns one Resource by id.
func (c *Client) GetResource(ctx context.Context, id string) (*Resource, error) {
	var out Resource
	err := c.do(ctx, http.MethodGet, "/api/v1/resources/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// DeleteResource deletes one Resource.
func (c *Client) DeleteResource(ctx context.Context, id string) (*Resource, error) {
	var out Resource
	err := c.do(ctx, http.MethodDelete, "/api/v1/resources/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// CreateResourceUpload submits a manifest and returns the objects the server
// still needs (api.md §3).
func (c *Client) CreateResourceUpload(ctx context.Context, id string, manifest any) (*Upload, error) {
	var out Upload
	err := c.do(ctx, http.MethodPost, "/api/v1/resources/"+url.PathEscape(id)+"/uploads", nil, manifest, &out, "")
	return &out, err
}

// PublishResourceUpload turns a completed upload into a Resource Version.
func (c *Client) PublishResourceUpload(ctx context.Context, id, uploadID string) (*ResourceVersion, error) {
	var out ResourceVersion
	err := c.do(ctx, http.MethodPost, "/api/v1/resources/"+url.PathEscape(id)+"/versions", nil,
		map[string]string{"upload_id": uploadID}, &out, "")
	return &out, err
}

// ListResourceVersions returns one page of a Resource's Versions.
func (c *Client) ListResourceVersions(ctx context.Context, id string, limit int, cursor string) (*ResourceVersionList, error) {
	var out ResourceVersionList
	err := c.do(ctx, http.MethodGet, "/api/v1/resources/"+url.PathEscape(id)+"/versions", pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// GetResourceVersion returns one Resource Version.
func (c *Client) GetResourceVersion(ctx context.Context, id, version string) (*ResourceVersion, error) {
	var out ResourceVersion
	err := c.do(ctx, http.MethodGet,
		"/api/v1/resources/"+url.PathEscape(id)+"/versions/"+url.PathEscape(version), nil, nil, &out, "")
	return &out, err
}

// ResourceManifest returns the normalized manifest verbatim.
func (c *Client) ResourceManifest(ctx context.Context, id, version string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet,
		"/api/v1/resources/"+url.PathEscape(id)+"/versions/"+url.PathEscape(version)+"/manifest", nil, nil, &out, "")
	return out, err
}

// CreateResourceDownloads returns presigned URLs for the requested digests.
func (c *Client) CreateResourceDownloads(ctx context.Context, id, version string, sha256 []string) (*DownloadURLs, error) {
	var out DownloadURLs
	err := c.do(ctx, http.MethodPost,
		"/api/v1/resources/"+url.PathEscape(id)+"/versions/"+url.PathEscape(version)+"/downloads", nil,
		map[string]any{"sha256": sha256}, &out, "")
	return &out, err
}

// SetResourceCurrent points a Resource at a Version; ifMatch guards against a
// concurrent change.
func (c *Client) SetResourceCurrent(ctx context.Context, id, version, ifMatch string) (*Resource, error) {
	var out Resource
	err := c.doWithHeaders(ctx, http.MethodPut, "/api/v1/resources/"+url.PathEscape(id)+"/current", nil,
		map[string]string{"version": version}, &out, "", map[string]string{"If-Match": ifMatch})
	return &out, err
}

// Workspaces.

// CreateWorkspace creates a persistent Workspace.
func (c *Client) CreateWorkspace(ctx context.Context, req CreateWorkspaceRequest, idempotencyKey string) (*Workspace, error) {
	var out Workspace
	err := c.do(ctx, http.MethodPost, "/api/v1/workspaces", nil, req, &out, idempotencyKey)
	return &out, err
}

// ListWorkspaces returns one page of Workspaces.
func (c *Client) ListWorkspaces(ctx context.Context, limit int, cursor string) (*WorkspaceList, error) {
	var out WorkspaceList
	err := c.do(ctx, http.MethodGet, "/api/v1/workspaces", pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// GetWorkspace returns one Workspace.
func (c *Client) GetWorkspace(ctx context.Context, id string) (*Workspace, error) {
	var out Workspace
	err := c.do(ctx, http.MethodGet, "/api/v1/workspaces/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// DeleteWorkspace deletes one Workspace.
func (c *Client) DeleteWorkspace(ctx context.Context, id string) (*Workspace, error) {
	var out Workspace
	err := c.do(ctx, http.MethodDelete, "/api/v1/workspaces/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// PublishWorkspace publishes a Workspace subtree as a Resource Version.
func (c *Client) PublishWorkspace(ctx context.Context, id, path, resource string) (*ResourceVersion, error) {
	var out ResourceVersion
	err := c.do(ctx, http.MethodPost, "/api/v1/workspaces/"+url.PathEscape(id)+"/publish", nil,
		map[string]string{"path": path, "resource": resource}, &out, "")
	return &out, err
}

// DeclareWorkspaceLost marks a Workspace lost after its Node dies.
func (c *Client) DeclareWorkspaceLost(ctx context.Context, id string) (*Workspace, error) {
	return c.workspaceAction(ctx, id, "declare-lost")
}

// AbandonWorkspace abandons a Workspace without waiting for its Node.
func (c *Client) AbandonWorkspace(ctx context.Context, id string) (*Workspace, error) {
	return c.workspaceAction(ctx, id, "abandon")
}

func (c *Client) workspaceAction(ctx context.Context, id, action string) (*Workspace, error) {
	var out Workspace
	err := c.do(ctx, http.MethodPost, "/api/v1/workspaces/"+url.PathEscape(id)+"/"+action, nil, nil, &out, "")
	return &out, err
}

// Snapshots.

// CreateSnapshot creates a Snapshot by name.
func (c *Client) CreateSnapshot(ctx context.Context, name string) (*Snapshot, error) {
	var out Snapshot
	err := c.do(ctx, http.MethodPost, "/api/v1/snapshots", nil, map[string]string{"name": name}, &out, "")
	return &out, err
}

// ListSnapshots returns one page of Snapshots.
func (c *Client) ListSnapshots(ctx context.Context, limit int, cursor string) (*SnapshotList, error) {
	var out SnapshotList
	err := c.do(ctx, http.MethodGet, "/api/v1/snapshots", pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// GetSnapshot returns one Snapshot.
func (c *Client) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	var out Snapshot
	err := c.do(ctx, http.MethodGet, "/api/v1/snapshots/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// DeleteSnapshot deletes one Snapshot.
func (c *Client) DeleteSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	var out Snapshot
	err := c.do(ctx, http.MethodDelete, "/api/v1/snapshots/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// CreateSnapshotVersion imports or builds a new Snapshot Version.
func (c *Client) CreateSnapshotVersion(ctx context.Context, id string, req SnapshotVersionRequest, idempotencyKey string) (*SnapshotVersion, error) {
	var out SnapshotVersion
	err := c.do(ctx, http.MethodPost, "/api/v1/snapshots/"+url.PathEscape(id)+"/versions", nil,
		req, &out, idempotencyKey)
	return &out, err
}

// GetSnapshotVersion returns one Snapshot Version.
func (c *Client) GetSnapshotVersion(ctx context.Context, id, version string) (*SnapshotVersion, error) {
	var out SnapshotVersion
	err := c.do(ctx, http.MethodGet,
		"/api/v1/snapshots/"+url.PathEscape(id)+"/versions/"+url.PathEscape(version), nil, nil, &out, "")
	return &out, err
}

// ListSnapshotVersions returns one page of a Snapshot's Versions.
func (c *Client) ListSnapshotVersions(ctx context.Context, id string, limit int, cursor string) (*SnapshotVersionList, error) {
	var out SnapshotVersionList
	err := c.do(ctx, http.MethodGet,
		"/api/v1/snapshots/"+url.PathEscape(id)+"/versions", pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// SetSnapshotCurrent points a Snapshot at a Version.
func (c *Client) SetSnapshotCurrent(ctx context.Context, id, version string) (*Snapshot, error) {
	var out Snapshot
	err := c.do(ctx, http.MethodPut, "/api/v1/snapshots/"+url.PathEscape(id)+"/current", nil,
		map[string]string{"version": version}, &out, "")
	return &out, err
}

// CancelSnapshotVersion cancels a pending Snapshot Version build or import.
func (c *Client) CancelSnapshotVersion(ctx context.Context, id, version string) (*SnapshotVersion, error) {
	var out SnapshotVersion
	err := c.do(ctx, http.MethodPost,
		"/api/v1/snapshots/"+url.PathEscape(id)+"/versions/"+url.PathEscape(version)+"/cancel", nil, nil, &out, "")
	return &out, err
}

// Sandboxes.

// CreateSandbox creates a Sandbox and returns it in its initial state.
func (c *Client) CreateSandbox(ctx context.Context, req CreateSandboxRequest, idempotencyKey string) (*Sandbox, error) {
	var out Sandbox
	err := c.do(ctx, http.MethodPost, "/api/v1/sandboxes", nil, req, &out, idempotencyKey)
	return &out, err
}

// ListSandboxes returns one filtered page of Sandboxes.
func (c *Client) ListSandboxes(ctx context.Context, params ListSandboxesParams) (*SandboxList, error) {
	q := pageQuery(params.Limit, params.Cursor)
	for name, value := range map[string]string{
		"owner": params.Owner, "state": params.State, "label": params.Label, "node": params.Node,
	} {
		if value != "" {
			q.Set(name, value)
		}
	}
	if params.HasGPU {
		q.Set("has_gpu", "true")
	}
	var out SandboxList
	err := c.do(ctx, http.MethodGet, "/api/v1/sandboxes", q, nil, &out, "")
	return &out, err
}

// GetSandbox reads one Sandbox. waitFor long-polls the state for up to 60
// seconds; an empty string returns immediately.
func (c *Client) GetSandbox(ctx context.Context, id, waitFor string) (*Sandbox, error) {
	q := url.Values{}
	if waitFor != "" {
		q.Set("wait_for", waitFor)
		// A shallow copy keeps the transport and only raises this call's
		// deadline past the server's 60-second long poll.
		hc := *c.http
		hc.Timeout = waitForTimeout
		var out Sandbox
		err := c.doWithClient(&hc, ctx, http.MethodGet, "/api/v1/sandboxes/"+url.PathEscape(id), q, nil, &out, "", nil)
		return &out, err
	}
	var out Sandbox
	err := c.do(ctx, http.MethodGet, "/api/v1/sandboxes/"+url.PathEscape(id), q, nil, &out, "")
	return &out, err
}

// DeleteSandbox deletes a Sandbox and cancels its running Execs.
func (c *Client) DeleteSandbox(ctx context.Context, id string) (*Sandbox, error) {
	return c.sandboxAction(ctx, id, "")
}

// StartSandbox starts a stopped Sandbox.
func (c *Client) StartSandbox(ctx context.Context, id string) (*Sandbox, error) {
	return c.sandboxAction(ctx, id, "start")
}

// StopSandbox stops a Sandbox and keeps its Workspace contents.
func (c *Client) StopSandbox(ctx context.Context, id string) (*Sandbox, error) {
	return c.sandboxAction(ctx, id, "stop")
}

func (c *Client) sandboxAction(ctx context.Context, id, action string) (*Sandbox, error) {
	path := "/api/v1/sandboxes/" + url.PathEscape(id)
	method := http.MethodDelete
	if action != "" {
		path += "/" + action
		method = http.MethodPost
	}
	var out Sandbox
	err := c.do(ctx, method, path, nil, nil, &out, "")
	return &out, err
}

// Execs.

// CreateExec starts a command in a Sandbox.
func (c *Client) CreateExec(ctx context.Context, sandboxID string, req ExecRequest, idempotencyKey string) (*Exec, error) {
	var out Exec
	err := c.do(ctx, http.MethodPost, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/execs", nil,
		req, &out, idempotencyKey)
	return &out, err
}

// GetExec returns one Exec.
func (c *Client) GetExec(ctx context.Context, id string) (*Exec, error) {
	var out Exec
	err := c.do(ctx, http.MethodGet, "/api/v1/execs/"+url.PathEscape(id), nil, nil, &out, "")
	return &out, err
}

// ListExecs returns one page of a Sandbox's Execs.
func (c *Client) ListExecs(ctx context.Context, sandboxID string, limit int, cursor string) (*ExecList, error) {
	var out ExecList
	err := c.do(ctx, http.MethodGet, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/execs",
		pageQuery(limit, cursor), nil, &out, "")
	return &out, err
}

// CancelExec cancels a running Exec.
func (c *Client) CancelExec(ctx context.Context, id string) (*Exec, error) {
	var out Exec
	err := c.do(ctx, http.MethodPost, "/api/v1/execs/"+url.PathEscape(id)+"/cancel", nil, nil, &out, "")
	return &out, err
}

// ExecOutput reads one page of an Execution Log starting at offset after.
func (c *Client) ExecOutput(ctx context.Context, id string, after int64) ([]Frame, error) {
	q := url.Values{"after": {strconv.FormatInt(after, 10)}}
	var out []Frame
	err := c.do(ctx, http.MethodGet, "/api/v1/execs/"+url.PathEscape(id)+"/output", q, nil, &out, "")
	return out, err
}

// Files and ports.

// ListFiles lists a Workspace directory (api.md §5).
func (c *Client) ListFiles(ctx context.Context, sandboxID, path string) (*FileList, error) {
	q := url.Values{"path": {path}}
	var out FileList
	err := c.do(ctx, http.MethodGet, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files", q, nil, &out, "")
	return &out, err
}

// StatFile reads one Workspace path's metadata (api.md §5).
func (c *Client) StatFile(ctx context.Context, sandboxID, path string) (*FileEntry, error) {
	var out FileEntry
	err := c.do(ctx, http.MethodGet, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files/stat",
		url.Values{"path": {path}}, nil, &out, "")
	return &out, err
}

// Mkdir creates a directory in the Workspace.
func (c *Client) Mkdir(ctx context.Context, sandboxID, path string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files/mkdir",
		nil, map[string]string{"path": path}, nil, "")
}

// MoveFile moves or renames a Workspace path.
func (c *Client) MoveFile(ctx context.Context, sandboxID, from, to string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files/move",
		nil, map[string]string{"from": from, "to": to}, nil, "")
}

// DeleteFile deletes a Workspace path.
func (c *Client) DeleteFile(ctx context.Context, sandboxID, path string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/files",
		url.Values{"path": {path}}, nil, nil, "")
}

// CreatePortTicket returns a short-lived URL for a Sandbox port.
func (c *Client) CreatePortTicket(ctx context.Context, sandboxID string, port int) (*PortTicket, error) {
	var out PortTicket
	err := c.do(ctx, http.MethodPost,
		"/api/v1/sandboxes/"+url.PathEscape(sandboxID)+"/ports/"+strconv.Itoa(port)+"/ticket",
		nil, nil, &out, "")
	return &out, err
}

// doWithHeaders is do with extra request headers (If-Match).
func (c *Client) doWithHeaders(ctx context.Context, method, path string, query url.Values, body, out any, idempotencyKey string, headers map[string]string) error {
	return c.doWithClient(c.http, ctx, method, path, query, body, out, idempotencyKey, headers)
}

// doWithClient is the single status-check and decode path; hc carries the
// per-call timeout.
func (c *Client) doWithClient(hc *http.Client, ctx context.Context, method, path string, query url.Values, body, out any, idempotencyKey string, headers map[string]string) error {
	status, data, err := c.sendHeaders(hc, ctx, method, path, query, body, idempotencyKey, headers)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return parseError(status, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
