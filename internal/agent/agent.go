// Package agent is the Node Agent: enrollment, the Control stream, and the
// Runtime capability self-check (docs/spec/node-protocol.md, runtime.md §9).
package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/buildinfo"
	"github.com/zibyn/orrery/internal/nodeproto"
)

const (
	// HeartbeatInterval is a constant in defaults.md.
	HeartbeatInterval = 15 * time.Second
	DefaultStatePath  = "/var/lib/orrery-agent/identity.json"
	DefaultDockerSock = "/var/run/docker.sock"
	// DefaultContainerdSock and DefaultSandboxSubnet are the containerd
	// Provider's defaults (runtime.md §9, defaults.md).
	DefaultContainerdSock = "/run/containerd/containerd.sock"
	DefaultSandboxSubnet  = "10.213.0.0/24"
	DefaultDataRoot       = "/var/lib/orrery"
	DefaultReserveCPU     = 1.0
	DefaultReserveBytes   = 2 << 30 // 2 GiB
	// DefaultDiskWatermark is the fraction of the disk that must stay free
	// (defaults.md, Node configuration).
	DefaultDiskWatermark = 0.10

	maxBackoff = time.Minute
)

// Identity is what `enroll` leaves behind for `run`. It is the Node's only
// secret and never leaves the host unencrypted.
type Identity struct {
	NodeURL   string `json:"node_url"`
	NodeID    string `json:"node_id"`
	NodeToken string `json:"node_token"`
}

type EnrollOptions struct {
	Server string
	CAPath string
	Packet string
	Name   string
	State  string
}

// Enroll redeems a single-use Enrollment Packet and stores the Node identity
// with mode 0600.
func Enroll(ctx context.Context, opts EnrollOptions) (Identity, error) {
	if opts.Packet == "" {
		return Identity{}, errors.New("--packet is required")
	}
	if opts.Server == "" {
		return Identity{}, errors.New("--server is required")
	}
	name := opts.Name
	if name == "" {
		host, err := os.Hostname()
		if err != nil {
			return Identity{}, fmt.Errorf("hostname: %w", err)
		}
		name = host
	}
	version := buildinfo.Version()
	creds, err := transportCredentials(opts.CAPath, opts.Server)
	if err != nil {
		return Identity{}, err
	}
	conn, err := grpc.NewClient(opts.Server, grpc.WithTransportCredentials(creds))
	if err != nil {
		return Identity{}, fmt.Errorf("dial %s: %w", opts.Server, err)
	}
	defer conn.Close() //nolint:errcheck // nothing to flush

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := nodev1.NewNodeClient(conn).Redeem(ctx, &nodev1.RedeemRequest{
		Packet:       opts.Packet,
		Name:         name,
		AgentVersion: version,
		ProtoMajor:   nodeproto.Major,
		ProtoMinor:   nodeproto.Minor,
	})
	if err != nil {
		return Identity{}, fmt.Errorf("redeem Enrollment Packet: %w", err)
	}
	id := Identity{NodeURL: opts.Server, NodeID: resp.NodeId, NodeToken: resp.NodeToken}
	if err := SaveIdentity(opts.State, id); err != nil {
		return Identity{}, err
	}
	return id, nil
}

func LoadIdentity(path string) (Identity, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("read identity %s (run enroll first): %w", path, err)
	}
	var id Identity
	if err := json.Unmarshal(raw, &id); err != nil {
		return Identity{}, fmt.Errorf("parse identity %s: %w", path, err)
	}
	if id.NodeID == "" || id.NodeToken == "" {
		return Identity{}, fmt.Errorf("identity %s is incomplete", path)
	}
	return id, nil
}

func SaveIdentity(path string, id Identity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}
	raw, err := json.Marshal(id)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("write identity: %w", err)
	}
	return nil
}

type RunOptions struct {
	CAPath string
	Server string // overrides the address stored at enroll time
	State  string
	Docker string
	// Containerd is the containerd socket and SandboxSubnet the CNI address
	// pool for --runtime containerd (runtime.md §9, §13).
	Containerd    string
	SandboxSubnet string
	// Runtime is the Node's one Runtime Provider (ADR-0014): docker, podman,
	// containerd or local.
	Runtime string
	// LocalUser is the unprivileged user every Local Runtime Exec runs as
	// (runtime.md §10). Required for --runtime local.
	LocalUser string
	// localUser is the resolved LocalUser, filled by Run.
	localUser *localUser
	// Buildkit is the BuildKit endpoint. Empty on a Docker Node means the Docker daemon's
	// built-in BuildKit, reached through the Docker socket's /grpc upgrade.
	Buildkit      string
	DataRoot      string
	ReserveCPU    float64
	ReserveMemory int64
	DiskWatermark float64
}

// resolveRuntime picks the one Runtime Provider this Node runs (ADR-0014).
// Unimplemented providers fail loudly rather than silently running Docker.
func resolveRuntime(name string) (string, error) {
	switch name {
	case "", ProviderDocker:
		return ProviderDocker, nil
	case ProviderPodman:
		return ProviderPodman, nil
	case ProviderContainerd:
		return ProviderContainerd, nil
	case ProviderLocal:
		return ProviderLocal, nil
	default:
		return "", fmt.Errorf("unknown runtime %q", name)
	}
}

// Run keeps one Control stream alive, probing capabilities once per
// connection. NOT_FOUND/UNAUTHENTICATED clears local identity; anything else
// keeps it and backs off (ADR-0010's asymmetric rule).
func Run(ctx context.Context, opts RunOptions) error {
	provider, err := resolveRuntime(opts.Runtime)
	if err != nil {
		return err
	}
	opts.Runtime = provider
	if provider == ProviderLocal {
		// Local Runtime uses the system manager and systemd-run, so the Agent
		// must be root; commands never run as root (runtime.md §10).
		if os.Geteuid() != 0 {
			return errors.New("runtime local requires the Agent to run as root (system manager)")
		}
		user, err := lookupLocalUser(opts.LocalUser)
		if err != nil {
			return err
		}
		opts.localUser = &user
	}
	id, err := LoadIdentity(opts.State)
	if err != nil {
		return err
	}
	server := opts.Server
	if server == "" {
		server = id.NodeURL
	}
	if server == "" {
		return errors.New("no node address: pass --server or re-enroll")
	}
	if opts.Docker == "" {
		opts.Docker = DefaultDockerSock
	}
	if opts.DataRoot == "" {
		opts.DataRoot = DefaultDataRoot
	}
	if opts.Containerd == "" {
		opts.Containerd = DefaultContainerdSock
	}
	if opts.SandboxSubnet == "" {
		opts.SandboxSubnet = DefaultSandboxSubnet
	}
	if opts.ReserveMemory == 0 {
		opts.ReserveMemory = DefaultReserveBytes
	}
	if opts.DiskWatermark <= 0 {
		opts.DiskWatermark = DefaultDiskWatermark
	}
	if err := PrepareDataRoot(opts.DataRoot); err != nil {
		return err
	}
	version := buildinfo.Version()

	backoff := time.Second
	for {
		connected, err := runOnce(ctx, server, id, opts, version)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && clearsIdentity(err) {
			if rmErr := os.Remove(opts.State); rmErr != nil && !os.IsNotExist(rmErr) {
				slog.Warn("identity was rejected and could not be removed", "error", rmErr)
			}
			return fmt.Errorf("identity rejected by the Control Plane, local identity cleared: %w", err)
		}
		if connected {
			backoff = time.Second
		}
		if err != nil {
			if isIncompatible(err) {
				slog.Error("Node protocol version incompatible; keep this Agent installed and upgrade the Control Plane first",
					"error", err)
			} else {
				slog.Warn("control stream ended", "error", err, "retry_in", backoff)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func clearsIdentity(err error) bool {
	switch status.Code(err) {
	case codes.NotFound, codes.Unauthenticated:
		return true
	default:
		return false
	}
}

func isIncompatible(err error) bool {
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.FailedPrecondition {
		return false
	}
	for _, detail := range st.Details() {
		if info, ok := detail.(*errdetails.ErrorInfo); ok && info.Reason == "incompatible" {
			return true
		}
	}
	return false
}

func runOnce(ctx context.Context, server string, id Identity, opts RunOptions, version string) (bool, error) {
	// The --runtime flag is the single source of truth: an explicit Provider
	// wins over any path heuristic, so a Docker Node pointing at an unusual
	// socket never silently becomes Podman.
	rt := NewRuntime(opts.Docker, opts.DataRoot, id.NodeID)
	if opts.Runtime == ProviderPodman {
		var err error
		rt, err = newPodmanRuntime(opts.Docker, opts.DataRoot, id.NodeID)
		if err != nil {
			return false, err
		}
	}
	if opts.Runtime == ProviderContainerd {
		var err error
		rt, err = newContainerdRuntime(opts.Containerd, opts.SandboxSubnet, opts.DataRoot, id.NodeID)
		if err != nil {
			return false, err
		}
		defer rt.driver.(*containerdDriver).close()
	}
	if opts.Runtime == ProviderLocal {
		if opts.localUser == nil {
			return false, errors.New("runtime local needs --local-user")
		}
		rt = newLocalRuntime(opts.DataRoot, id.NodeID, *opts.localUser, agentBinDir())
	}
	rt.buildkit = opts.Buildkit
	creds, err := transportCredentials(opts.CAPath, server)
	if err != nil {
		return false, err
	}
	conn, err := grpc.NewClient(server, grpc.WithTransportCredentials(creds))
	if err != nil {
		return false, err
	}
	defer conn.Close() //nolint:errcheck // stream ends first

	streamCtx, cancel := context.WithCancel(metadata.AppendToOutgoingContext(ctx,
		"authorization", "Bearer "+id.NodeToken))
	defer cancel()
	stream, err := nodev1.NewNodeClient(conn).Control(streamCtx)
	if err != nil {
		return false, err
	}
	if err := stream.Send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_Hello{
		Hello: &nodev1.Hello{
			NodeId:       id.NodeID,
			AgentVersion: version,
			ProtoMajor:   nodeproto.Major,
			ProtoMinor:   nodeproto.Minor,
		},
	}}); err != nil {
		return false, err
	}
	first, err := stream.Recv()
	if err != nil {
		return false, err
	}
	ack := first.GetHelloAck()
	if ack == nil || !ack.Accepted {
		return false, errors.New("the Control Plane rejected Hello")
	}
	rt.setRegistryCredential(ack.RegistryCredential)
	slog.Info("control stream connected", "node_id", id.NodeID)

	// One goroutine owns Send; the receive loop and command goroutines only
	// queue messages, so the stream is never written concurrently.
	out := make(chan *nodev1.AgentMessage, 128)
	sendErr := make(chan error, 1)
	send := func(msg *nodev1.AgentMessage) {
		select {
		case out <- msg:
		case <-streamCtx.Done():
		}
	}
	go func() {
		for {
			select {
			case <-streamCtx.Done():
				return
			case msg := <-out:
				if err := stream.Send(msg); err != nil {
					select {
					case sendErr <- err:
					default:
					}
					return
				}
			}
		}
	}()

	reportSandbox := func(sandboxID string, generation int64, phase nodeproto.SandboxPhase, cause string) {
		send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_SandboxObserved{
			SandboxObserved: &nodev1.SandboxObserved{
				SandboxId: sandboxID, Generation: generation, Phase: string(phase), Cause: cause,
			},
		}})
	}

	// snapshot_build is the BuildKit endpoint self-check, independent of the
	// Runtime (runtime.md §12). It is reported even when the Runtime probe
	// fails; the Node's status already keeps it out of placement then.
	buildOK := rt.buildkitAvailable(streamCtx)
	report, probeErr := rt.probe(streamCtx)
	health := "healthy"
	if probeErr != nil {
		health = "unhealthy"
		report = nodeproto.CapabilityReport{RuntimeProvider: rt.provider}
		slog.Error("runtime self-check failed", "error", probeErr)
	}
	report = withSnapshotBuild(report, buildOK)
	// The inventory is a hardware fact; placement still requires the CDI
	// capability above, so an old Docker's GPUs are visible but not usable.
	gpus := enumerateGPUs(streamCtx)
	send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_Capabilities{
		Capabilities: capabilitiesMessage(report, gpus),
	}})
	host := hostHardware()
	heartbeat := func() *nodev1.AgentMessage {
		return heartbeatMessage(health, host, opts, gpuHealth(streamCtx, gpus))
	}
	send(heartbeat())

	runner := newCommandRunner(streamCtx, rt, send)
	rt.connect(nodev1.NewNodeClient(conn), send)
	// A build a previous process (or a dropped stream) left behind is
	// interrupted, never retried (runtime.md §12).
	go rt.resumeBuilds(streamCtx)
	// Cache eviction owns its own loop (runtime.md §11): a Node below the disk
	// watermark reclaims LRU cache without touching Sandbox lifecycle.
	go rt.evictLoop(streamCtx, opts.DiskWatermark)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			runner.handle(msg)
		}
	}()

	// An Agent restart must not look like a dead runtime: pick up containers
	// this Node still runs.
	go rt.adopt(streamCtx, func(sandboxID string, generation int64) {
		reportSandbox(sandboxID, generation, nodeproto.SandboxPhaseRunning, "")
	})
	// A Runtime that died takes its Specs with it: end the local records first
	// so their deliveries end with an exit frame.
	reportGone := func(sandboxID string, generation int64) {
		rt.finalizeLost(sandboxID)
		reportSandbox(sandboxID, generation, nodeproto.SandboxPhaseGone, "")
	}
	go rt.watchEvents(streamCtx, reportGone)
	// Repair what a previous Agent process, or an earlier connection, left
	// unfinished (runtime.md §8).
	go rt.reconcileRecords(streamCtx, reportGone)

	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return true, nil
		case err := <-recvErr:
			return true, err
		case err := <-sendErr:
			return true, err
		case <-ticker.C:
			// Runtime health is live: a daemon that dies mid-stream must stop
			// receiving placements, not keep its startup verdict.
			hctx, hcancel := context.WithTimeout(streamCtx, 5*time.Second)
			if rt.healthy(hctx) {
				health = "healthy"
			} else {
				health = "unhealthy"
			}
			hcancel()
			send(heartbeat())
			// A container that died on a live Node is reported once.
			go rt.scan(streamCtx, reportGone)
		}
	}
}

type hardware struct {
	CPUCores    float64
	MemoryBytes int64
}

func hostHardware() hardware {
	var info syscall.Sysinfo_t
	var memory int64
	if err := syscall.Sysinfo(&info); err == nil {
		memory = int64(info.Totalram) * int64(info.Unit)
	}
	return hardware{CPUCores: float64(runtime.NumCPU()), MemoryBytes: memory}
}

func heartbeatMessage(health string, host hardware, opts RunOptions, gpuStates []*nodev1.GPUHealth) *nodev1.AgentMessage {
	free, total := diskUsage("/")
	return &nodev1.AgentMessage{Message: &nodev1.AgentMessage_Heartbeat{
		Heartbeat: &nodev1.Heartbeat{
			RuntimeHealth:  health,
			DiskFree:       free,
			DiskTotal:      total,
			DiskWatermark:  opts.DiskWatermark,
			CpuCores:       host.CPUCores,
			MemoryBytes:    uint64(host.MemoryBytes),
			ReservedCpu:    opts.ReserveCPU,
			ReservedMemory: uint64(opts.ReserveMemory),
			GpuHealth:      gpuStates,
		},
	}}
}

func capabilitiesMessage(snapshot nodeproto.CapabilityReport, gpus []gpuInfo) *nodev1.Capabilities {
	caps := &nodev1.Capabilities{RuntimeProvider: snapshot.RuntimeProvider}
	for _, entry := range snapshot.Entries {
		caps.Entries = append(caps.Entries, &nodev1.CapabilityEntry{
			Name:  entry.Name,
			Value: capabilityProto(entry.Value),
		})
	}
	for _, gpu := range gpus {
		caps.Gpus = append(caps.Gpus, &nodev1.GPU{Uuid: gpu.UUID, Model: gpu.Model})
	}
	return caps
}

func capabilityProto(value string) nodev1.CapabilityValue {
	switch value {
	case nodeproto.ValueEnforced:
		return nodev1.CapabilityValue_CAPABILITY_VALUE_ENFORCED
	case nodeproto.ValueBestEffort:
		return nodev1.CapabilityValue_CAPABILITY_VALUE_BEST_EFFORT
	default:
		return nodev1.CapabilityValue_CAPABILITY_VALUE_UNSUPPORTED
	}
}

func transportCredentials(caPath, server string) (credentials.TransportCredentials, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName(server)}
	if caPath != "" {
		pem, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read CA %s: %w", caPath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("CA %s contains no certificates", caPath)
		}
		cfg.RootCAs = pool
	}
	return credentials.NewTLS(cfg), nil
}

func serverName(server string) string {
	host := server
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	return strings.Trim(host, "[]")
}

func diskUsage(path string) (free, total uint64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0
	}
	return stat.Bavail * uint64(stat.Bsize), stat.Blocks * uint64(stat.Bsize)
}
