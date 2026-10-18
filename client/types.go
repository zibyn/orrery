package client

import (
	"encoding/json"
	"time"
)

// Quota is a User's hard limit for GPUs and Sandboxes.
type Quota struct {
	GPUs      int32 `json:"gpus"`
	Sandboxes int32 `json:"sandboxes"`
}

type User struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Email     *string    `json:"email"`
	IsAdmin   bool       `json:"is_admin"`
	LoginType string     `json:"login_type"`
	Quota     Quota      `json:"quota"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at"`
}

type UserList struct {
	Items      []User  `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type QuotaUsageItem struct {
	Limit int32 `json:"limit"`
	Used  int32 `json:"used"`
}

type UserQuota struct {
	GPUs      QuotaUsageItem `json:"gpus"`
	Sandboxes QuotaUsageItem `json:"sandboxes"`
}

type Token struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

type CreatedToken struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Scope      string     `json:"scope"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
	Token      string     `json:"token"`
}

type TokenList struct {
	Items      []Token `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type Secret struct {
	Name      string    `json:"name"`
	UpdatedAt time.Time `json:"updated_at"`
}

type SecretList struct {
	Items      []Secret `json:"items"`
	NextCursor *string  `json:"next_cursor"`
}

type Audit struct {
	ID            int64           `json:"id"`
	At            time.Time       `json:"at"`
	ActorUserID   *string         `json:"actor_user_id"`
	ActorName     *string         `json:"actor_name"`
	ActorTokenID  *string         `json:"actor_token_id"`
	Action        string          `json:"action"`
	TargetType    string          `json:"target_type"`
	TargetID      string          `json:"target_id"`
	CorrelationID *string         `json:"correlation_id"`
	Detail        json.RawMessage `json:"detail"`
}

type AuditList struct {
	Items      []Audit `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type GitLink struct {
	ProviderID string    `json:"provider_id"`
	LinkedAt   time.Time `json:"linked_at"`
}

type GitLinkList struct {
	Items      []GitLink `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

type GitCredential struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type EnrollmentPacket struct {
	ID        string    `json:"id"`
	Packet    string    `json:"packet"`
	ExpiresAt time.Time `json:"expires_at"`
}

type GPU struct {
	UUID  string `json:"uuid"`
	Model string `json:"model"`
}

type Node struct {
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Status        string          `json:"status"`
	Paused        bool            `json:"paused"`
	LostAt        *time.Time      `json:"lost_at"`
	AgentVersion  *string         `json:"agent_version"`
	ProtoMajor    *int32          `json:"proto_major"`
	ProtoMinor    *int32          `json:"proto_minor"`
	LastSeenAt    *time.Time      `json:"last_seen_at"`
	RuntimeHealth *string         `json:"runtime_health"`
	Capabilities  json.RawMessage `json:"capabilities"`
	Hardware      json.RawMessage `json:"hardware"`
	GPUs          []GPU           `json:"gpus"`
	CreatedAt     time.Time       `json:"created_at"`
}

type NodeList struct {
	Items      []Node  `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type CapacityGPU struct {
	Model     string `json:"model"`
	Total     int    `json:"total"`
	Allocated int    `json:"allocated"`
}

type CapacityNode struct {
	NodeID string        `json:"node_id"`
	Name   string        `json:"name"`
	Status string        `json:"status"`
	GPUs   []CapacityGPU `json:"gpus"`
}

type CapacityList struct {
	Items      []CapacityNode `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

type Resource struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	CurrentVersionID *string    `json:"current_version_id"`
	CreatedAt        time.Time  `json:"created_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
}

type ResourceList struct {
	Items      []Resource `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

type ResourceVersion struct {
	ID         string          `json:"id"`
	ResourceID string          `json:"resource_id"`
	Address    *string         `json:"address"`
	Size       int64           `json:"size"`
	State      string          `json:"state"`
	Cause      *string         `json:"cause"`
	Provenance json.RawMessage `json:"provenance"`
	CreatedAt  time.Time       `json:"created_at"`
	ExpiresAt  *time.Time      `json:"expires_at"`
}

type ResourceVersionList struct {
	Items      []ResourceVersion `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

type UploadPut struct {
	SHA256 string `json:"sha256"`
	URL    string `json:"url"`
}

type Upload struct {
	UploadID  string      `json:"upload_id"`
	ExpiresAt time.Time   `json:"expires_at"`
	Puts      []UploadPut `json:"puts"`
}

type DownloadURLs struct {
	URLs []string `json:"urls"`
}

type Workspace struct {
	ID          string     `json:"id"`
	Kind        string     `json:"kind"`
	NodeID      *string    `json:"node_id"`
	State       string     `json:"state"`
	Cause       *string    `json:"cause"`
	Unavailable bool       `json:"unavailable"`
	Conditions  []string   `json:"conditions"`
	SandboxID   *string    `json:"sandbox_id"`
	CreatedAt   time.Time  `json:"created_at"`
	DeletedAt   *time.Time `json:"deleted_at"`
}

type WorkspaceList struct {
	Items      []Workspace `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

type Snapshot struct {
	ID               string     `json:"id"`
	OwnerID          *string    `json:"owner_id"`
	Name             string     `json:"name"`
	CurrentVersionID *string    `json:"current_version_id"`
	CreatedAt        *time.Time `json:"created_at"`
	DeletedAt        *time.Time `json:"deleted_at,omitempty"`
}

type SnapshotList struct {
	Items      []Snapshot `json:"items"`
	NextCursor *string    `json:"next_cursor"`
}

type SnapshotVersion struct {
	ID             string          `json:"id"`
	SnapshotID     string          `json:"snapshot_id"`
	Digest         *string         `json:"digest"`
	State          string          `json:"state"`
	Cause          *string         `json:"cause"`
	Source         string          `json:"source"`
	BuildSandboxID *string         `json:"build_sandbox_id,omitempty"`
	BuildExecID    *string         `json:"build_exec_id,omitempty"`
	Provenance     json.RawMessage `json:"provenance,omitempty"`
	CreatedAt      *time.Time      `json:"created_at"`
}

type SnapshotVersionList struct {
	Items      []SnapshotVersion `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

type SnapshotImport struct {
	Ref        string  `json:"ref"`
	AuthSecret *string `json:"auth_secret,omitempty"`
}

type SnapshotBuild struct {
	Dockerfile string            `json:"dockerfile"`
	Base       *string           `json:"base,omitempty"`
	Contexts   map[string]string `json:"contexts,omitempty"`
	Secrets    map[string]string `json:"secrets,omitempty"`
}

type SnapshotVersionRequest struct {
	Import *SnapshotImport `json:"import,omitempty"`
	Build  *SnapshotBuild  `json:"build,omitempty"`
}

type SandboxWorkspace struct {
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

type SandboxResource struct {
	Resource string  `json:"resource"`
	Version  *string `json:"version,omitempty"`
	Mount    *string `json:"mount,omitempty"`
}

type SandboxGPU struct {
	Count int32   `json:"count"`
	Model *string `json:"model,omitempty"`
}

type CreateSandboxRequest struct {
	Name      *string           `json:"name,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Runtime   *string           `json:"runtime,omitempty"`
	Workspace *SandboxWorkspace `json:"workspace,omitempty"`
	Snapshot  *string           `json:"snapshot,omitempty"`
	Resources []SandboxResource `json:"resources,omitempty"`
	Secrets   map[string]string `json:"secrets,omitempty"`
	CPU       *float64          `json:"cpu,omitempty"`
	Memory    *int64            `json:"memory,omitempty"`
	GPU       *SandboxGPU       `json:"gpu,omitempty"`
	IdleStop  *int32            `json:"idle_stop,omitempty"`
}

type Sandbox struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Labels        map[string]string `json:"labels"`
	Runtime       string            `json:"runtime"`
	State         string            `json:"state"`
	Cause         *string           `json:"cause"`
	NodeID        *string           `json:"node_id"`
	Snapshot      string            `json:"snapshot"`
	CPU           float64           `json:"cpu"`
	Memory        int64             `json:"memory"`
	GPU           *SandboxGPU       `json:"gpu,omitempty"`
	IdleStop      int32             `json:"idle_stop"`
	Workspace     *SandboxWorkspace `json:"workspace,omitempty"`
	Conditions    []string          `json:"conditions"`
	QueuePosition *int64            `json:"queue_position,omitempty"`
	BlockedReason *string           `json:"blocked_reason,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

type SandboxList struct {
	Items      []Sandbox `json:"items"`
	NextCursor *string   `json:"next_cursor"`
}

// PTY is the terminal size requested when creating a PTY Exec.
type PTY struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

type ExecRequest struct {
	Argv    []string          `json:"argv"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout *int64            `json:"timeout,omitempty"`
	Stdin   *string           `json:"stdin,omitempty"`
	PTY     *PTY              `json:"pty,omitempty"`
}

type ExecResult struct {
	Cause    string `json:"cause"`
	ExitCode *int32 `json:"exit_code,omitempty"`
	Signal   *int32 `json:"signal,omitempty"`
}

type Exec struct {
	ID           string      `json:"id"`
	SandboxID    string      `json:"sandbox_id"`
	Argv         []string    `json:"argv"`
	Cwd          string      `json:"cwd"`
	Timeout      *int32      `json:"timeout"`
	State        string      `json:"state"`
	Result       *ExecResult `json:"result"`
	LogTruncated bool        `json:"log_truncated"`
	AcceptedAt   time.Time   `json:"accepted_at"`
	StartedAt    *time.Time  `json:"started_at"`
	FinishedAt   *time.Time  `json:"finished_at"`
	DeadlineAt   *time.Time  `json:"deadline_at"`
}

type ExecList struct {
	Items      []Exec  `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// Frame is one page of an Execution Log (api.md §4).
type Frame struct {
	Seq      int64  `json:"seq"`
	Stream   string `json:"stream"`
	Data     string `json:"data"`
	Encoding string `json:"encoding,omitempty"`
}

type FileEntry struct {
	Path       string    `json:"path"`
	Name       string    `json:"name"`
	Type       string    `json:"type"`
	Size       int64     `json:"size"`
	Mode       uint32    `json:"mode"`
	ModTime    time.Time `json:"mod_time"`
	LinkTarget *string   `json:"link_target,omitempty"`
}

type FileList struct {
	Items []FileEntry `json:"items"`
}

type PortTicket struct {
	URL string `json:"url"`
}

// Request bodies.

type SessionRequest struct {
	Username string        `json:"username"`
	Password string        `json:"password"`
	Token    *TokenRequest `json:"token,omitempty"`
}

type TokenRequest struct {
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type CreateUserRequest struct {
	Name      string  `json:"name"`
	Email     *string `json:"email,omitempty"`
	LoginType string  `json:"login_type"`
	Password  *string `json:"password,omitempty"`
	IsAdmin   bool    `json:"is_admin"`
}

type UpdateUserRequest struct {
	Name      *string `json:"name,omitempty"`
	Email     *string `json:"email,omitempty"`
	LoginType *string `json:"login_type,omitempty"`
	Password  *string `json:"password,omitempty"`
	IsAdmin   *bool   `json:"is_admin,omitempty"`
}

type GitCredentialRequest struct {
	Host     string `json:"host,omitempty"`
	Provider string `json:"provider,omitempty"`
}

type AuditParams struct {
	Actor      string
	Action     string
	Target     string
	TargetType string
	From       string
	To         string
	Limit      int
	Cursor     string
}

type ListSandboxesParams struct {
	Owner  string
	State  string
	Label  string
	Node   string
	HasGPU bool
	Limit  int
	Cursor string
}

type CreateWorkspaceRequest struct {
	Kind string `json:"kind,omitempty"`
}
