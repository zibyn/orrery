package api

import (
	"net/http"
	"strings"

	"github.com/zibyn/orrery/web"
)

type handlerFunc func(http.ResponseWriter, *http.Request, *principal)

type route struct {
	Method        string
	Path          string // without the /api/v1 prefix
	Auth          string // "", "user", or "admin"
	OperationID   string
	Summary       string
	Request       string // schema name, or empty
	Response      string // schema name, or empty
	Query         []string
	SuccessStatus int
	Handler       handlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{http.MethodGet, "/users", "admin", "listUsers", "List Users", "", "UserList", []string{"limit", "cursor"}, http.StatusOK, s.handleListUsers},
		{http.MethodGet, "/users/{id}", "admin", "getUser", "Get a User", "", "User", nil, http.StatusOK, s.handleGetUser},
		{http.MethodPatch, "/users/{id}", "admin", "updateUser", "Update a User", "UpdateUserRequest", "User", nil, http.StatusOK, s.handleUpdateUser},
		{http.MethodDelete, "/users/{id}", "admin", "deleteUser", "Delete a User", "", "User", nil, http.StatusAccepted, s.handleDeleteUser},
		{http.MethodPut, "/users/{id}/quota", "admin", "setUserQuota", "Set a User's quota", "SetQuotaRequest", "User", nil, http.StatusOK, s.handleSetUserQuota},
		{http.MethodGet, "/users/me", "user", "getCurrentUser", "Get the calling User", "", "User", nil, http.StatusOK, s.handleGetMe},
		{http.MethodGet, "/users/me/quota", "user", "getCurrentUserQuota", "Get the calling User's quota and usage", "", "UserQuotaUsage", nil, http.StatusOK, s.handleGetMyQuota},
		{http.MethodPost, "/users/me/tokens", "user", "createToken", "Create an API Token", "CreateTokenRequest", "CreatedToken", nil, http.StatusCreated, s.handleCreateToken},
		{http.MethodGet, "/users/me/tokens", "user", "listTokens", "List API Tokens", "", "TokenList", []string{"limit", "cursor"}, http.StatusOK, s.handleListTokens},
		{http.MethodDelete, "/users/me/tokens/{id}", "user", "revokeToken", "Revoke an API Token", "", "Token", nil, http.StatusAccepted, s.handleRevokeToken},
		{http.MethodPut, "/secrets/{name}", "user", "putSecret", "Create or overwrite a Secret", "PutSecretRequest", "Secret", nil, http.StatusOK, s.handlePutSecret},
		{http.MethodGet, "/secrets", "user", "listSecrets", "List Secrets", "", "SecretList", []string{"limit", "cursor"}, http.StatusOK, s.handleListSecrets},
		{http.MethodDelete, "/secrets/{name}", "user", "deleteSecret", "Delete a Secret", "", "Secret", nil, http.StatusAccepted, s.handleDeleteSecret},
		{http.MethodPost, "/cli-logins", "", "createCLILogin", "Start a CLI device login", "", "CLILogin", nil, http.StatusCreated, s.handleCreateCLILogin},
		{http.MethodPost, "/cli-logins/approve", "user", "approveCLILogin", "Approve a CLI login by its user code", "CLILoginApproveRequest", "", nil, http.StatusNoContent, s.handleApproveCLILogin},
		{http.MethodPost, "/cli-logins/token", "", "collectCLILoginToken", "Poll a CLI login for its API Token", "CLILoginTokenRequest", "CLILoginToken", nil, http.StatusOK, s.handleCLILoginToken},
		{http.MethodDelete, "/sessions/current", "user", "deleteCurrentSession", "Log out the current Session", "", "Token", nil, http.StatusAccepted, s.handleDeleteCurrentSession},
		{http.MethodGet, "/oidc/login", "", "oidcLogin", "Start an OIDC login", "", "", []string{"return"}, http.StatusFound, s.handleOIDCLogin},
		{http.MethodGet, "/oidc/callback", "", "oidcCallback", "Finish an OIDC login", "", "", []string{"code", "state", "error"}, http.StatusFound, s.handleOIDCCallback},
		{http.MethodGet, "/oidc/account", "", "oidcAccount", "Open the Polaris account center", "", "", nil, http.StatusFound, s.handleOIDCAccount},
		{http.MethodPost, "/polaris/backchannel-logout", "", "polarisBackchannelLogout", "Receive a Polaris Back-Channel Logout", "BackchannelLogoutForm", "", nil, http.StatusOK, s.handleBackchannelLogout},
		{http.MethodPost, "/polaris/webhook", "", "polarisWebhook", "Receive a Polaris User event", "PolarisWebhookEvent", "", nil, http.StatusNoContent, s.handlePolarisWebhook},
		{http.MethodGet, "/audit", "admin", "listAudit", "List Audit Records", "", "AuditList", []string{"actor", "action", "target", "target_type", "from", "to", "limit", "cursor"}, http.StatusOK, s.handleListAudit},
		{http.MethodPost, "/enrollment-packets", "admin", "createEnrollmentPacket", "Issue an Enrollment Packet", "", "EnrollmentPacket", nil, http.StatusCreated, s.handleCreateEnrollmentPacket},
		{http.MethodGet, "/nodes", "admin", "listNodes", "List Nodes", "", "NodeList", []string{"limit", "cursor"}, http.StatusOK, s.handleListNodes},
		{http.MethodGet, "/nodes/{id}", "admin", "getNode", "Get a Node", "", "Node", nil, http.StatusOK, s.handleGetNode},
		{http.MethodDelete, "/nodes/{id}", "admin", "deleteNode", "Delete a Node", "", "Node", nil, http.StatusAccepted, s.handleDeleteNode},
		{http.MethodPost, "/nodes/{id}/pause", "admin", "pauseNode", "Pause a Node", "", "Node", nil, http.StatusAccepted, s.handlePauseNode},
		{http.MethodPost, "/nodes/{id}/resume", "admin", "resumeNode", "Resume a Node", "", "Node", nil, http.StatusAccepted, s.handleResumeNode},
		{http.MethodPost, "/nodes/{id}/declare-lost", "admin", "declareNodeLost", "Declare a Node lost", "", "Node", nil, http.StatusAccepted, s.handleDeclareNodeLost},
		{http.MethodGet, "/capacity", "user", "getCapacity", "Get Node capacity", "", "CapacityList", nil, http.StatusOK, s.handleGetCapacity},
		{http.MethodPost, "/resources", "user", "createResource", "Create a Resource", "CreateResourceRequest", "Resource", nil, http.StatusCreated, s.handleCreateResource},
		{http.MethodGet, "/resources", "user", "listResources", "List Resources", "", "ResourceList", []string{"limit", "cursor"}, http.StatusOK, s.handleListResources},
		{http.MethodGet, "/resources/{id}", "user", "getResource", "Get a Resource", "", "Resource", nil, http.StatusOK, s.handleGetResource},
		{http.MethodDelete, "/resources/{id}", "user", "deleteResource", "Delete a Resource", "", "Resource", nil, http.StatusAccepted, s.handleDeleteResource},
		{http.MethodPost, "/resources/{id}/uploads", "user", "createResourceUpload", "Open an upload session", "Manifest", "Upload", nil, http.StatusCreated, s.handleCreateUpload},
		{http.MethodPost, "/resources/{id}/versions", "user", "createResourceVersion", "Publish a ResourceVersion", "CreateResourceVersionRequest", "ResourceVersion", nil, http.StatusCreated, s.handleCreateResourceVersion},
		{http.MethodGet, "/resources/{id}/versions", "user", "listResourceVersions", "List ResourceVersions", "", "ResourceVersionList", []string{"limit", "cursor"}, http.StatusOK, s.handleListResourceVersions},
		{http.MethodGet, "/resources/{id}/versions/{v}", "user", "getResourceVersion", "Get a ResourceVersion", "", "ResourceVersion", nil, http.StatusOK, s.handleGetResourceVersion},
		{http.MethodGet, "/resources/{id}/versions/{v}/manifest", "user", "getResourceManifest", "Get a ResourceVersion manifest", "", "Manifest", nil, http.StatusOK, s.handleGetResourceManifest},
		{http.MethodPost, "/resources/{id}/versions/{v}/downloads", "user", "createResourceDownloads", "Presign object downloads", "ResourceDownloadsRequest", "ResourceDownloads", nil, http.StatusOK, s.handleCreateResourceDownloads},
		{http.MethodPut, "/resources/{id}/current", "user", "setResourceCurrent", "Move the current version pointer", "SetResourceCurrentRequest", "Resource", nil, http.StatusOK, s.handleSetResourceCurrent},
		{http.MethodPost, "/volumes", "user", "createVolume", "Create a Volume", "CreateVolumeRequest", "Volume", nil, http.StatusCreated, s.handleCreateVolume},
		{http.MethodGet, "/volumes", "user", "listVolumes", "List Volumes", "", "VolumeList", []string{"limit", "cursor"}, http.StatusOK, s.handleListVolumes},
		{http.MethodGet, "/volumes/{id}", "user", "getVolume", "Get a Volume", "", "Volume", nil, http.StatusOK, s.handleGetVolume},
		{http.MethodDelete, "/volumes/{id}", "user", "deleteVolume", "Delete a Volume", "", "Volume", nil, http.StatusAccepted, s.handleDeleteVolume},
		{http.MethodGet, "/volumes/{id}/files", "user", "listVolumeFiles", "List a Volume directory as of its last commit", "", "FileList", []string{"path"}, http.StatusOK, s.handleListVolumeFiles},
		{http.MethodGet, "/volumes/{id}/files/stat", "user", "statVolumeFile", "Stat a Volume path as of its last commit", "", "FileEntry", []string{"path"}, http.StatusOK, s.handleStatVolumeFile},
		{http.MethodGet, "/volumes/{id}/files/content", "user", "downloadVolumeFile", "Download a Volume file as of its last commit", "", "", []string{"path"}, http.StatusOK, s.handleVolumeFileContent},
		{http.MethodGet, "/volumes/{id}/files/archive", "user", "downloadVolumeArchive", "Download a Volume subtree as tar as of its last commit", "", "", []string{"path"}, http.StatusOK, s.handleVolumeArchive},
		{http.MethodPost, "/volumes/{id}/publish", "user", "publishVolume", "Publish a Volume path as a Resource version", "PublishRequest", "ResourceVersion", nil, http.StatusAccepted, s.handlePublishVolume},
		{http.MethodPost, "/snapshots", "user", "createSnapshot", "Create a Snapshot", "CreateSnapshotRequest", "Snapshot", nil, http.StatusCreated, s.handleCreateSnapshot},
		{http.MethodGet, "/snapshots", "user", "listSnapshots", "List own and platform Snapshots", "", "SnapshotList", []string{"limit", "cursor"}, http.StatusOK, s.handleListSnapshots},
		{http.MethodGet, "/snapshots/{id}", "user", "getSnapshot", "Get a Snapshot", "", "Snapshot", nil, http.StatusOK, s.handleGetSnapshot},
		{http.MethodDelete, "/snapshots/{id}", "user", "deleteSnapshot", "Delete a Snapshot", "", "Snapshot", nil, http.StatusAccepted, s.handleDeleteSnapshot},
		{http.MethodPost, "/snapshots/{id}/versions", "user", "createSnapshotVersion", "Import or build a SnapshotVersion", "CreateSnapshotVersionRequest", "SnapshotVersion", nil, http.StatusCreated, s.handleCreateSnapshotVersion},
		{http.MethodGet, "/snapshots/{id}/versions", "user", "listSnapshotVersions", "List SnapshotVersions", "", "SnapshotVersionList", []string{"limit", "cursor"}, http.StatusOK, s.handleListSnapshotVersions},
		{http.MethodGet, "/snapshots/{id}/versions/{v}", "user", "getSnapshotVersion", "Get a SnapshotVersion", "", "SnapshotVersion", nil, http.StatusOK, s.handleGetSnapshotVersion},
		{http.MethodDelete, "/snapshots/{id}/versions/{v}", "user", "deleteSnapshotVersion", "Delete a SnapshotVersion", "", "SnapshotVersion", nil, http.StatusAccepted, s.handleDeleteSnapshotVersion},
		{http.MethodPost, "/snapshots/{id}/versions/{v}/cancel", "user", "cancelSnapshotVersion", "Cancel an import or build", "", "SnapshotVersion", nil, http.StatusAccepted, s.handleCancelSnapshotVersion},
		{http.MethodPut, "/snapshots/{id}/current", "user", "setSnapshotCurrent", "Move a Snapshot's current pointer", "SetSnapshotCurrentRequest", "Snapshot", nil, http.StatusOK, s.handleSetSnapshotCurrent},
		{http.MethodPost, "/sandboxes", "user", "createSandbox", "Create a Sandbox", "CreateSandboxRequest", "Sandbox", nil, http.StatusCreated, s.handleCreateSandbox},
		{http.MethodGet, "/sandboxes", "user", "listSandboxes", "List Sandboxes", "", "SandboxList", []string{"owner", "state", "label", "node", "has_gpu", "limit", "cursor"}, http.StatusOK, s.handleListSandboxes},
		{http.MethodGet, "/sandboxes/{id}", "user", "getSandbox", "Get a Sandbox", "", "Sandbox", []string{"wait_for"}, http.StatusOK, s.handleGetSandbox},
		{http.MethodDelete, "/sandboxes/{id}", "user", "deleteSandbox", "Delete a Sandbox", "", "Sandbox", nil, http.StatusAccepted, s.handleDeleteSandbox},
		{http.MethodPost, "/sandboxes/{id}/publish", "user", "publishSandbox", "Publish a path of a ready Sandbox as a Resource version", "PublishRequest", "ResourceVersion", nil, http.StatusAccepted, s.handlePublishSandbox},
		{http.MethodPost, "/sandboxes/{id}/start", "user", "startSandbox", "Start a Sandbox", "", "Sandbox", nil, http.StatusAccepted, s.handleStartSandbox},
		{http.MethodPost, "/sandboxes/{id}/stop", "user", "stopSandbox", "Stop a Sandbox", "", "Sandbox", nil, http.StatusAccepted, s.handleStopSandbox},
		{http.MethodPost, "/sandboxes/{id}/execs", "user", "createExec", "Run a command in a Sandbox", "CreateExecRequest", "Exec", nil, http.StatusCreated, s.handleCreateExec},
		{http.MethodGet, "/sandboxes/{id}/execs", "user", "listExecs", "List a Sandbox's Execs", "", "ExecList", []string{"limit", "cursor"}, http.StatusOK, s.handleListExecs},
		{http.MethodGet, "/execs/{id}", "user", "getExec", "Get an Exec", "", "Exec", nil, http.StatusOK, s.handleGetExec},
		{http.MethodPost, "/execs/{id}/cancel", "user", "cancelExec", "Cancel an Exec", "", "Exec", nil, http.StatusAccepted, s.handleCancelExec},
		{http.MethodGet, "/execs/{id}/output", "user", "getExecOutput", "Read an Exec's output", "", "ExecOutput", []string{"after", "follow"}, http.StatusOK, s.handleExecOutput},
		{http.MethodGet, "/execs/{id}/pty", "user", "attachExecPTY", "Attach to an Exec's PTY", "", "", nil, http.StatusSwitchingProtocols, s.handleExecPTY},
		{http.MethodGet, "/sandboxes/{id}/files", "user", "listSandboxFiles", "List Sandbox files", "", "FileList", []string{"path"}, http.StatusOK, s.fileHandler("")},
		{http.MethodGet, "/sandboxes/{id}/files/stat", "user", "statSandboxFile", "Stat a Sandbox path", "", "FileEntry", []string{"path"}, http.StatusOK, s.fileHandler("/stat")},
		{http.MethodGet, "/sandboxes/{id}/files/content", "user", "downloadSandboxFile", "Download a Sandbox file", "", "", []string{"path"}, http.StatusOK, s.fileHandler("/content")},
		{http.MethodPut, "/sandboxes/{id}/files/content", "user", "uploadSandboxFile", "Upload a Sandbox file", "", "", []string{"path"}, http.StatusNoContent, s.fileHandler("/content")},
		{http.MethodPost, "/sandboxes/{id}/files/mkdir", "user", "createSandboxDirectory", "Create a Sandbox directory", "FileMkdirRequest", "", nil, http.StatusNoContent, s.fileHandler("/mkdir")},
		{http.MethodPost, "/sandboxes/{id}/files/move", "user", "moveSandboxFile", "Move a Sandbox path", "FileMoveRequest", "", nil, http.StatusNoContent, s.fileHandler("/move")},
		{http.MethodDelete, "/sandboxes/{id}/files", "user", "deleteSandboxFile", "Delete a Sandbox path", "", "", []string{"path"}, http.StatusNoContent, s.fileHandler("")},
		{http.MethodGet, "/sandboxes/{id}/files/archive", "user", "downloadSandboxArchive", "Download a Sandbox subtree as tar", "", "", []string{"path"}, http.StatusOK, s.fileHandler("/archive")},
		{http.MethodPut, "/sandboxes/{id}/files/archive", "user", "uploadSandboxArchive", "Unpack a tar into the Sandbox", "", "", []string{"path"}, http.StatusNoContent, s.fileHandler("/archive")},
		{http.MethodPost, "/sandboxes/{id}/ports/{port}/ticket", "user", "createSandboxPortTicket", "Issue a port-forward ticket", "", "PortTicket", nil, http.StatusOK, s.handlePortTicket},
		{http.MethodGet, "/sandboxes/{id}/ports/{port}/tunnel", "user", "openSandboxPortTunnel", "Forward a Sandbox port over a WebSocket", "", "", nil, http.StatusSwitchingProtocols, s.handlePortTunnel},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		h := rt.Handler
		var handler http.HandlerFunc
		switch rt.Auth {
		case "admin", "user":
			handler = s.wrapAuth(rt.Auth, h)
		default:
			handler = func(w http.ResponseWriter, r *http.Request) { h(w, r, nil) }
		}
		mux.HandleFunc(rt.Method+" /api/v1"+rt.Path, handler)
	}
	// Unknown paths and methods answer with problem+json; the closed code
	// catalog has no 405, so a wrong method is reported as not found.
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := mux.Handler(r); pattern == "" {
			writeProblem(w, "not_found", "no such endpoint")
			return
		}
		mux.ServeHTTP(w, r)
	})
	// A port subdomain is the Gateway, not the API (api.md §6). The API
	// prefix and /metrics stay on the API listener; every other path is the
	// Web console (api.md §1, §8).
	console := consoleHandler(web.FS())
	return s.withRequestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id, port, ok := parseAppHost(r.Host, s.cfg.AppsDomain); ok {
			s.servePortGateway(w, r, id, port)
			return
		}
		if r.URL.Path == "/api/v1" || strings.HasPrefix(r.URL.Path, "/api/v1/") || r.URL.Path == "/metrics" {
			api.ServeHTTP(w, r)
			return
		}
		console.ServeHTTP(w, r)
	}))
}
