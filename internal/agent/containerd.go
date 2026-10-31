package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	eventtypes "github.com/containerd/containerd/api/events"
	runtimeoptions "github.com/containerd/containerd/api/types/runtimeoptions/v1"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/defaults"
	cdispec "github.com/containerd/containerd/v2/pkg/cdi"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	"github.com/distribution/reference"
	"github.com/google/uuid"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"tags.cncf.io/container-device-interface/pkg/cdi"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
	"github.com/zibyn/orrery/internal/supervisor"
)

// containerdNamespace holds every object this Agent creates (runtime.md §9).
const containerdNamespace = "orrery"

// containerdDriver is the containerd Runtime Provider under runc (runtime.md
// §9, ADR-0026). It shares every host-path operation with the Local Runtime.
type containerdDriver struct {
	r      *Runtime
	client *containerd.Client
	subnet string
	// runscConfig is the runsc options TOML on a runsc Node; empty under runc.
	runscConfig string
	// probing is the name of the probe container now running (a string), which
	// adopt, running alongside the self-check, must leave alone.
	probing atomic.Value
	// networkMu serialises renders of the nftables table and guards probes,
	// the self-check's networks, which are never recorded on disk.
	networkMu sync.Mutex
	probes    map[uuid.UUID]sandboxNetwork
	// links is the latest SandboxLinks set, in memory only: the Control
	// Plane resends it on every reconnect. Guarded by networkMu.
	links []*nodev1.SandboxLink
	// probeLink is the sandbox_links self-check's Link, rendered beside links
	// while it runs. Guarded by networkMu.
	probeLink *nodev1.SandboxLink
}

// runsc reports whether this Node runs Sandboxes under runsc.
func (d *containerdDriver) runsc() bool { return d.runscConfig != "" }

// newContainerdRuntime is the explicit constructor for --runtime containerd.
func newContainerdRuntime(socket, subnet, isolation, dataRoot, nodeID string) (*Runtime, error) {
	client, err := containerd.New(socket, containerd.WithDefaultNamespace(containerdNamespace))
	if err != nil {
		return nil, fmt.Errorf("connect to containerd: %w", err)
	}
	// The CDI registry is refreshed on each injection, as ctr does, instead
	// of watching the spec directories (runtime.md §9).
	if err := cdi.Configure(cdi.WithAutoRefresh(false)); err != nil {
		return nil, fmt.Errorf("configure CDI: %w", err)
	}
	rt := newRuntime(ProviderContainerd, dataRoot, nodeID)
	driver := &containerdDriver{r: rt, client: client, subnet: subnet}
	if isolation == IsolationRunsc {
		// The guest has no OOM counter; the Agent reads the host's (§9).
		rt.hostCgroup = "/sys/fs/cgroup"
		// Sockets the Supervisor creates (PTY ctl.sock, forwards) must be
		// visible on the host (runtime.md §9).
		driver.runscConfig = filepath.Join(dataRoot, "runsc.toml")
		if err := os.WriteFile(driver.runscConfig, []byte("[runsc_config]\n  host-uds = \"create\"\n"), 0o644); err != nil {
			client.Close() //nolint:errcheck // failed constructor
			return nil, fmt.Errorf("write runsc options: %w", err)
		}
	}
	rt.driver = driver
	return rt, nil
}

func (d *containerdDriver) close() { d.client.Close() } //nolint:errcheck // shutdown

func (d *containerdDriver) resolver() remotes.Resolver {
	d.r.registryMu.Lock()
	cred := d.r.registry
	d.r.registryMu.Unlock()
	auth := docker.NewDockerAuthorizer(docker.WithAuthCreds(func(host string) (string, string, error) {
		cfg, _ := cred.Get(host)
		return cfg.Username, cfg.Password, nil
	}))
	// Loopback registries speak plain HTTP, as dockerd treats them.
	return docker.NewResolver(docker.ResolverOptions{Hosts: docker.ConfigureDefaultRegistries(
		docker.WithAuthorizer(auth), docker.WithPlainHTTP(docker.MatchLocalhost))})
}

// image returns the unpacked image, pulling it with the Node's registry
// credential when it is not in the namespace yet.
func (d *containerdDriver) image(ctx context.Context, ref string) (containerd.Image, error) {
	named, err := reference.ParseDockerRef(ref)
	if err != nil {
		return nil, err
	}
	if img, err := d.client.GetImage(ctx, named.String()); err == nil {
		if ok, err := img.IsUnpacked(ctx, defaults.DefaultSnapshotter); err == nil && ok {
			return img, nil
		}
		return img, img.Unpack(ctx, defaults.DefaultSnapshotter)
	}
	return d.client.Pull(ctx, named.String(), containerd.WithPullUnpack, containerd.WithResolver(d.resolver()))
}

func bindMount(source, destination, mode string) specs.Mount {
	return specs.Mount{Type: "bind", Source: source, Destination: destination, Options: []string{"rbind", mode}}
}

// limitOpts sets the three hard limits with the same CPU rounding as the
// Local Runtime (ADR-0009), in a private cgroup namespace whose cgroupfs is
// mounted read-only: the Supervisor reads memory.events there for oom_killed
// (runtime.md §4).
func limitOpts(cpu float64, memory int64) []oci.SpecOpts {
	return []oci.SpecOpts{
		oci.WithCPUCFS(localQuota(cpu), 100000),
		oci.WithMemoryLimit(uint64(memory)),
		// No swap on top: unset, runc leaves memory.swap.max at max and a
		// Sandbox at its limit swaps instead of being OOM-killed.
		oci.WithMemorySwap(memory),
		oci.WithPidsLimit(sandboxPidsLimit),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.CgroupNamespace}),
		oci.WithMounts([]specs.Mount{{
			Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup",
			Options: []string{"ro", "nosuid", "noexec", "nodev"},
		}}),
	}
}

// start creates the container and starts its task.
func (d *containerdDriver) start(ctx context.Context, name string, img containerd.Image, labels map[string]string, opts []oci.SpecOpts) (containerd.Container, error) {
	containerOpts := []containerd.NewContainerOpts{
		containerd.WithImage(img),
		containerd.WithNewSnapshot(name, img),
		containerd.WithContainerLabels(labels),
		containerd.WithNewSpec(append([]oci.SpecOpts{oci.WithImageConfig(img)}, opts...)...),
	}
	if d.runsc() {
		containerOpts = append(containerOpts, containerd.WithRuntime("io.containerd.runsc.v1",
			&runtimeoptions.Options{TypeUrl: "io.containerd.runsc.v1.options", ConfigPath: d.runscConfig}))
	}
	c, err := d.client.NewContainer(ctx, name, containerOpts...)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}
	task, err := c.NewTask(ctx, cio.NullIO)
	if err != nil {
		return c, fmt.Errorf("create task: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		return c, fmt.Errorf("start task: %w", err)
	}
	return c, nil
}

// hostLimitsMatch reads the limits back from the host cgroup (runtime.md §9).
func hostLimitsMatch(ctx context.Context, c containerd.Container, cpu float64, memory int64) bool {
	spec, err := c.Spec(ctx)
	if err != nil || spec.Linux == nil {
		return false
	}
	return localLimitsMatch(filepath.Join("/sys/fs/cgroup", spec.Linux.CgroupsPath), cpu, memory)
}

func (d *containerdDriver) createSandbox(ctx context.Context, cmd *createSandboxSpec) (string, error) {
	id, err := uuid.Parse(cmd.sandboxID)
	if err != nil {
		return "runtime_create_failed", err
	}
	workspaceID, err := uuid.Parse(cmd.workspaceID)
	if err != nil {
		return "runtime_create_failed", err
	}
	// runsc would turn on nvproxy for NVIDIA devices instead of failing (runtime.md §9).
	if d.runsc() && len(cmd.gpuUUIDs) > 0 {
		return "runtime_create_failed", errors.New("this runsc Node does not attach GPUs")
	}
	policy, err := d.r.egressPolicy(cmd.egress)
	if err != nil {
		return "runtime_create_failed", err
	}
	img, err := d.image(ctx, cmd.imageDigest)
	if err != nil {
		return "image_pull_failed", err
	}
	workspace, runDir, mode, err := d.r.prepareSandboxHost(id, workspaceID, cmd)
	if err != nil {
		return "runtime_create_failed", err
	}
	name := containerName(id)
	if c, err := d.client.LoadContainer(ctx, name); err == nil {
		labels, err := c.Labels(ctx)
		if running, _ := taskRunning(ctx, c); running && err == nil &&
			createAdopts(labels, d.r.nodeID, cmd.generation) && hostLimitsMatch(ctx, c, cmd.cpu, cmd.memory) {
			d.r.track(id, cmd.generation)
			return "", nil // adopted after an Agent restart or a lost reply
		}
	}
	// A stopped Runtime is never reused, and neither is its netns.
	if err := d.teardown(ctx, id); err != nil {
		return "runtime_create_failed", err
	}
	resources, cause, err := d.r.resourceMounts(ctx, cmd.resources)
	if err != nil {
		return cause, err
	}
	if _, err = d.setupNetwork(ctx, id, sandboxPath(d.r.dataRoot, id), policy); err == nil {
		err = d.applyNetwork(ctx)
	}
	if err != nil {
		d.teardown(context.WithoutCancel(ctx), id) //nolint:errcheck // best effort
		return "runtime_create_failed", err
	}
	etc := filepath.Join(sandboxPath(d.r.dataRoot, id), "etc")
	mounts := []specs.Mount{
		bindMount(workspace, workspaceMount, mode),
		bindMount(filepath.Join(d.r.dataRoot, "bin"), binaryDir, "ro"),
		bindMount(runDir, runMount, "rw"),
		bindMount(filepath.Join(etc, "resolv.conf"), "/etc/resolv.conf", "ro"),
		bindMount(filepath.Join(etc, "hosts"), "/etc/hosts", "ro"),
		bindMount(filepath.Join(etc, "hostname"), "/etc/hostname", "ro"),
	}
	mounts = append(mounts, resources...)
	opts := append([]oci.SpecOpts{
		oci.WithProcessArgs(supervisorBin, "init"),
		oci.WithProcessCwd(workspaceMount),
		oci.WithHostname(id.String()),
		oci.WithMounts(mounts),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: netnsPath(id)}),
	}, limitOpts(cmd.cpu, cmd.memory)...)
	// Last: CDI adds env, hooks and mounts that earlier options would reset.
	opts = append(opts, cdispec.WithCDIDevices(cdiDeviceNames(cmd.gpuUUIDs)...))
	c, err := d.start(ctx, name, img, map[string]string{
		labelSandboxID:  id.String(),
		labelGeneration: strconv.FormatInt(cmd.generation, 10),
		labelManaged:    "true",
		labelNode:       d.r.nodeID,
	}, opts)
	if err == nil && !hostLimitsMatch(ctx, c, cmd.cpu, cmd.memory) {
		err = errors.New("host cgroup readback does not match the request")
	}
	if err == nil {
		var result supervisor.Result
		if result, err = d.execOutput(ctx, c, supervisorBin, "probe", workspaceMount); err == nil &&
			numberedDeviceCount(result.NvidiaDevices) != len(cmd.gpuUUIDs) {
			err = fmt.Errorf("the Sandbox sees GPU devices %v, want %d cards", result.NvidiaDevices, len(cmd.gpuUUIDs))
		}
	}
	if err != nil {
		d.teardown(context.WithoutCancel(ctx), id) //nolint:errcheck // best effort
		return "runtime_create_failed", err
	}
	d.r.track(id, cmd.generation)
	return "", nil
}

// removeContainer kills and deletes the task, the container and its
// snapshot. A missing object is already removed.
func (d *containerdDriver) removeContainer(ctx context.Context, name string) error {
	c, err := d.client.LoadContainer(ctx, name)
	if err == nil {
		if task, err := c.Task(ctx, nil); err == nil {
			if _, err := task.Delete(ctx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
				return fmt.Errorf("delete task: %w", err)
			}
		} else if !errdefs.IsNotFound(err) {
			return err
		}
		if err := c.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
			return fmt.Errorf("delete container: %w", err)
		}
	} else if !errdefs.IsNotFound(err) {
		return err
	}
	// A create that failed before the container existed can leave its snapshot.
	if err := d.client.SnapshotService(defaults.DefaultSnapshotter).Remove(ctx, name); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove snapshot: %w", err)
	}
	return nil
}

// teardown removes the Runtime, then its network (runtime.md §13).
func (d *containerdDriver) teardown(ctx context.Context, id uuid.UUID) error {
	if err := d.removeContainer(ctx, containerName(id)); err != nil {
		return err
	}
	return d.teardownNetwork(ctx, id, sandboxPath(d.r.dataRoot, id))
}

func (d *containerdDriver) stopSandbox(ctx context.Context, sandboxID uuid.UUID) error {
	if err := d.teardown(ctx, sandboxID); err != nil {
		return err
	}
	d.r.forget(sandboxID.String())
	return nil
}

func taskRunning(ctx context.Context, c containerd.Container) (bool, error) {
	task, err := c.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	status, err := task.Status(ctx)
	if errdefs.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status.Status == containerd.Running, nil
}

// execDetached starts args in the Sandbox with no stdio attached: the
// Supervisor's record, not the exec stream, carries the output (ADR-0009).
func (d *containerdDriver) execDetached(ctx context.Context, sandboxID uuid.UUID, execID string, args ...string) (*specs.Spec, error) {
	c, err := d.client.LoadContainer(ctx, containerName(sandboxID))
	if err != nil {
		return nil, fmt.Errorf("load container: %w", err)
	}
	spec, err := c.Spec(ctx)
	if err != nil {
		return nil, err
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("load task: %w", err)
	}
	process := *spec.Process
	process.Args = args
	process.Terminal = false
	p, err := task.Exec(ctx, execID, &process, cio.NullIO)
	if errdefs.IsAlreadyExists(err) {
		return spec, nil // a resent StartExec; the Supervisor's mkdir keeps it single
	}
	if err != nil {
		return nil, fmt.Errorf("create exec: %w", err)
	}
	if err := p.Start(ctx); err != nil {
		p.Delete(context.WithoutCancel(ctx)) //nolint:errcheck // best effort
		return nil, fmt.Errorf("start exec: %w", err)
	}
	// The shim keeps an exited exec until it is deleted; reap it while this
	// Agent runs. One left by a restart goes with the task on Stop.
	go func() {
		ctx := context.WithoutCancel(ctx)
		if status, err := p.Wait(ctx); err == nil {
			<-status
		}
		p.Delete(ctx) //nolint:errcheck // best effort
	}()
	return spec, nil
}

func (d *containerdDriver) startExec(ctx context.Context, cmd *nodev1.StartExec) error {
	sandboxID, err := uuid.Parse(cmd.SandboxId)
	if err != nil {
		return errors.New("invalid sandbox id")
	}
	execID, err := uuid.Parse(cmd.ExecId)
	if err != nil {
		return errors.New("invalid exec id")
	}
	slog.Info("start exec", "exec_id", execID, "sandbox_id", sandboxID)
	dir := filepath.Join(sandboxRunPath(d.r.dataRoot, sandboxID), execID.String())
	// Reconnect replay: the record still holds every frame still owed.
	if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
		go d.r.streamExec(ctx, sandboxID, execID, dir)
		return nil
	}
	spec, err := d.execDetached(ctx, sandboxID, execID.String(), supervisorBin, "run", execID.String())
	if err != nil {
		return err
	}
	imagePath := ""
	for _, entry := range spec.Process.Env {
		if path, ok := strings.CutPrefix(entry, "PATH="); ok {
			imagePath = path
		}
	}
	return d.r.deliverExecSpec(ctx, sandboxID, execID, dir, cmd,
		execEnv(cmd.Env, imagePath, binaryDir), cmd.Cwd)
}

func (d *containerdDriver) startForward(ctx context.Context, sandboxID uuid.UUID, args ...string) error {
	_, err := d.execDetached(ctx, sandboxID, "forward-"+uuid.NewString(), args...)
	return err
}

// probe runs one probe container through the same create path and reads its
// limits back from the host cgroup (runtime.md §9).
func (d *containerdDriver) probe(ctx context.Context) (nodeproto.CapabilityReport, error) {
	img, err := d.image(ctx, probeImage)
	if err != nil {
		return nodeproto.CapabilityReport{}, fmt.Errorf("probe image: %w", err)
	}
	// In the data root, which a runsc Node keeps off tmpfs (runtime.md §9).
	dir, err := os.MkdirTemp(d.r.dataRoot, "probe-")
	if err != nil {
		return nodeproto.CapabilityReport{}, err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best effort
	binDir, workspace := filepath.Join(dir, "bin"), filepath.Join(dir, "workspace")
	for _, sub := range []string{binDir, workspace} {
		if err := os.Mkdir(sub, 0o755); err != nil {
			return nodeproto.CapabilityReport{}, err
		}
	}
	self, err := os.Executable()
	if err != nil {
		return nodeproto.CapabilityReport{}, err
	}
	if err := copyFile(self, filepath.Join(binDir, "supervisor")); err != nil {
		return nodeproto.CapabilityReport{}, err
	}
	// The probe asks CDI for every card in the host inventory; a broken CDI
	// setup must not hide the other capabilities, so it falls back to none.
	uuids := gpuUUIDs(enumerateGPUs(ctx))
	// A runsc Node never attaches cards, so gpu_device_visibility stays unsupported.
	attempts := [][]string{nil}
	if len(uuids) > 0 && !d.runsc() {
		attempts = [][]string{uuids, nil}
	}
	var result supervisor.Result
	var limitsEnforced, gpuVisible, egressEnforced, linksEnforced bool
	for _, attempt := range attempts {
		result, limitsEnforced, egressEnforced, linksEnforced, err = d.probeOnce(ctx, img, dir, attempt)
		if err == nil {
			gpuVisible = len(attempt) > 0 && numberedDeviceCount(result.NvidiaDevices) == len(uuids)
			break
		}
		slog.Warn("probe container failed", "gpus", len(attempt), "error", err)
	}
	if err != nil {
		return nodeproto.CapabilityReport{}, err
	}
	return capabilities(result, limitsEnforced, gpuVisible, egressEnforced, linksEnforced, d.runsc()), nil
}

// probeOnce runs one probe container with the given cards attached, in dir
// (bin/ and workspace/ inside). It reports the limits readback and the
// egress and Link self-checks.
func (d *containerdDriver) probeOnce(ctx context.Context, img containerd.Image, dir string, gpus []string) (supervisor.Result, bool, bool, bool, error) {
	binDir, workspace := filepath.Join(dir, "bin"), filepath.Join(dir, "workspace")
	const cpu = 0.5
	id := uuid.New()
	name := "orrery-probe-" + id.String()
	// The egress self-check starts at Mode none, without the operator's Rules
	// (runtime.md §13).
	none, _ := resolveEgressPolicy(nil, nil, egressMode{name: nodeproto.EgressNone})
	network, err := d.setupNetwork(ctx, id, dir, none)
	defer d.teardownNetwork(context.WithoutCancel(ctx), id, dir) //nolint:errcheck // disposable
	if err == nil {
		d.setProbe(id, &network)
		err = d.applyNetwork(ctx)
	}
	if err != nil {
		return supervisor.Result{}, false, false, false, fmt.Errorf("probe network: %w", err)
	}
	opts := append([]oci.SpecOpts{
		oci.WithProcessArgs(probeSupervisor, "init"),
		oci.WithProcessCwd(probeWorkspace),
		oci.WithMounts([]specs.Mount{
			bindMount(workspace, probeWorkspace, "rw"),
			bindMount(binDir, binaryDir, "ro"),
		}),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: netnsPath(id)}),
	}, limitOpts(cpu, probeMemory)...)
	opts = append(opts, cdispec.WithCDIDevices(cdiDeviceNames(gpus)...))
	// Labelled managed but owned by no Node, so a probe a crash left behind is
	// removed with its network by the next adopt.
	d.probing.Store(name)
	defer d.probing.Store("")
	c, err := d.start(ctx, name, img, map[string]string{labelManaged: "true", labelSandboxID: id.String()}, opts)
	defer d.removeContainer(context.WithoutCancel(ctx), name) //nolint:errcheck // disposable
	if err != nil {
		return supervisor.Result{}, false, false, false, err
	}
	limitsEnforced := hostLimitsMatch(ctx, c, cpu, probeMemory)
	result, err := d.execOutput(ctx, c, probeSupervisor, "probe", probeWorkspace)
	if err != nil {
		return result, limitsEnforced, false, false, err
	}
	egressEnforced := d.egressEnforced(ctx, c, id, network)
	return result, limitsEnforced, egressEnforced, d.linksEnforced(ctx, img, dir, name, c, id, network), nil
}

// linksEnforced is the sandbox_links self-check (runtime.md §13): a second
// probe listens as a Link target linked only to the first, which must
// connect, while the Agent dialling it from the bridge gateway must not.
func (d *containerdDriver) linksEnforced(ctx context.Context, img containerd.Image, dir, sourceName string, source containerd.Container, sourceID uuid.UUID, sourceNet sandboxNetwork) bool {
	targetDir := filepath.Join(dir, "target")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		slog.Warn("link self-check: target dir", "error", err)
		return false
	}
	id := uuid.New()
	none, _ := resolveEgressPolicy(nil, nil, egressMode{name: nodeproto.EgressNone})
	network, err := d.setupNetwork(ctx, id, targetDir, none)
	defer d.teardownNetwork(context.WithoutCancel(ctx), id, targetDir) //nolint:errcheck // disposable
	if err != nil {
		slog.Warn("link self-check: target network", "error", err)
		return false
	}
	target := netip.AddrPortFrom(netip.MustParseAddr(network.Address), 8080).String()
	d.networkMu.Lock()
	d.probeLink = &nodev1.SandboxLink{TargetId: id.String(), TargetAddress: network.Address,
		SourceId: sourceID.String(), SourceAddress: sourceNet.Address, Ports: []uint32{8080}}
	d.networkMu.Unlock()
	// Runs before the teardown above, whose render then drops the Link.
	defer func() {
		d.networkMu.Lock()
		d.probeLink = nil
		d.networkMu.Unlock()
	}()
	d.setProbe(id, &network)
	if err := d.applyNetwork(ctx); err != nil {
		slog.Warn("link self-check: apply the Link", "error", err)
		return false
	}
	// Named after the source probe so adopt leaves it alone too.
	name := sourceName + "-target"
	_, err = d.start(ctx, name, img, map[string]string{labelManaged: "true", labelSandboxID: id.String()}, append([]oci.SpecOpts{
		oci.WithProcessArgs(probeSupervisor, "listen", "0.0.0.0:8080"),
		oci.WithMounts([]specs.Mount{bindMount(filepath.Join(dir, "bin"), binaryDir, "ro")}),
		oci.WithLinuxNamespace(specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: netnsPath(id)}),
	}, limitOpts(0.5, probeMemory)...))
	defer d.removeContainer(context.WithoutCancel(ctx), name) //nolint:errcheck // disposable
	if err != nil {
		slog.Warn("link self-check: start the target", "error", err)
		return false
	}
	// The target may still be binding, so the linked dial gets a few tries;
	// once it connects, the listener is up and the gateway's refusal counts.
	linked := false
	for attempt := 0; attempt < 10 && !linked; attempt++ {
		if attempt > 0 {
			time.Sleep(300 * time.Millisecond)
		}
		code, _, _, err := d.execProbe(ctx, source, fmt.Sprintf("link-%d", attempt), probeSupervisor, "dial", target)
		linked = err == nil && code == 0
	}
	if !linked {
		slog.Warn("link self-check: the linked probe did not reach the target")
		return false
	}
	// The host reaches the subnet from the bridge gateway address. Only a
	// timeout counts as refused: the Link chain drops, and any other error
	// says nothing about it.
	conn, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err == nil {
		conn.Close() //nolint:errcheck // the connect is the whole check
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		slog.Warn("link self-check: the bridge gateway was not dropped", "error", err)
		return false
	}
	return true
}

// egressEnforced is the egress_policy self-check (runtime.md §13): under
// Mode none the probe cannot reach a port the Agent listens on at the bridge
// gateway, and under an allowlist of just that port it can.
func (d *containerdDriver) egressEnforced(ctx context.Context, c containerd.Container, id uuid.UUID, network sandboxNetwork) bool {
	subnet, err := netip.ParsePrefix(d.subnet)
	if err != nil {
		return false
	}
	// host-local's default gateway, which the bridge plugin puts on the bridge.
	listener, err := net.Listen("tcp", netip.AddrPortFrom(subnet.Masked().Addr().Next(), 0).String())
	if err != nil {
		slog.Warn("egress self-check: listen on the bridge gateway", "error", err)
		return false
	}
	defer listener.Close() //nolint:errcheck // probe lifetime
	target := listener.Addr().String()
	if code, _, _, err := d.execProbe(ctx, c, "egress-none", probeSupervisor, "dial", target); err != nil || code == 0 {
		slog.Warn("egress self-check: Mode none reached the gateway", "code", code, "error", err)
		return false
	}
	addrPort := netip.MustParseAddrPort(target)
	allowlist, err := resolveEgressPolicy(nil, nil, egressMode{name: nodeproto.EgressAllowlist,
		allow: []string{fmt.Sprintf("%s/32:%d", addrPort.Addr(), addrPort.Port())}})
	if err == nil {
		network.Policy = allowlist
		d.setProbe(id, &network)
		err = d.applyNetwork(ctx)
	}
	if err != nil {
		slog.Warn("egress self-check: apply the allowlist", "error", err)
		return false
	}
	if code, _, stderr, err := d.execProbe(ctx, c, "egress-allowlist", probeSupervisor, "dial", target); err != nil || code != 0 {
		slog.Warn("egress self-check: the allowlist did not reach the gateway", "code", code, "error", err, "stderr", stderr)
		return false
	}
	return true
}

// execOutput runs one synchronous exec and decodes its JSON stdout.
func (d *containerdDriver) execOutput(ctx context.Context, c containerd.Container, args ...string) (supervisor.Result, error) {
	code, stdout, stderr, err := d.execProbe(ctx, c, "probe", args...)
	if err == nil && code != 0 {
		err = fmt.Errorf("probe exited %d: %s", code, stderr)
	}
	if err != nil {
		return supervisor.Result{}, err
	}
	var result supervisor.Result
	if err := json.Unmarshal(stdout, &result); err != nil {
		return supervisor.Result{}, fmt.Errorf("decode probe output: %w", err)
	}
	return result, nil
}

// execProbe runs one synchronous exec and returns its exit code and output.
func (d *containerdDriver) execProbe(ctx context.Context, c containerd.Container, execID string, args ...string) (int, []byte, string, error) {
	spec, err := c.Spec(ctx)
	if err != nil {
		return 0, nil, "", err
	}
	task, err := c.Task(ctx, nil)
	if err != nil {
		return 0, nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	process := *spec.Process
	process.Args = args
	var stdout, stderr bytes.Buffer
	p, err := task.Exec(ctx, execID, &process, cio.NewCreator(cio.WithStreams(nil, &stdout, &stderr)))
	if err != nil {
		return 0, nil, "", fmt.Errorf("create probe exec: %w", err)
	}
	defer p.Delete(context.WithoutCancel(ctx)) //nolint:errcheck // best effort
	statusC, err := p.Wait(ctx)
	if err != nil {
		return 0, nil, "", err
	}
	if err := p.Start(ctx); err != nil {
		return 0, nil, "", fmt.Errorf("start probe exec: %w", err)
	}
	status := <-statusC
	p.IO().Wait()
	code, _, err := status.Result()
	return int(code), stdout.Bytes(), strings.TrimSpace(stderr.String()), err
}

func (d *containerdDriver) healthy(ctx context.Context) bool {
	ok, err := d.client.IsServing(ctx)
	return err == nil && ok
}

// adopt takes over this Node's running Sandboxes and removes every other
// managed container in the namespace with its network (runtime.md §8).
func (d *containerdDriver) adopt(ctx context.Context, report func(string, int64)) {
	containers, err := d.client.Containers(ctx, `labels."`+labelManaged+`"==true`)
	if err != nil {
		return
	}
	for _, c := range containers {
		labels, err := c.Labels(ctx)
		if err != nil {
			continue
		}
		id, parseErr := uuid.Parse(labels[labelSandboxID])
		if !containerOwnedBy(labels, d.r.nodeID) {
			if probing, _ := d.probing.Load().(string); probing != "" && strings.HasPrefix(c.ID(), probing) {
				continue // this Agent's own self-check, removed when it ends
			}
			// The Workspace directory stays untouched: declaring it lost may be
			// wrong, and the data may have no other copy.
			slog.Info("removing a Runtime left by another Node",
				"container_id", c.ID(), "node", labels[labelNode])
			if err := d.removeContainer(ctx, c.ID()); err != nil {
				slog.Warn("remove foreign Runtime", "container_id", c.ID(), "error", err)
			} else if parseErr == nil {
				if err := d.teardownNetwork(ctx, id, sandboxPath(d.r.dataRoot, id)); err != nil {
					slog.Warn("remove foreign Runtime network", "container_id", c.ID(), "error", err)
				}
			}
			continue
		}
		gen, err := strconv.ParseInt(labels[labelGeneration], 10, 64)
		if err != nil || parseErr != nil {
			continue
		}
		if running, err := taskRunning(ctx, c); err != nil || !running {
			continue
		}
		d.r.track(id, gen)
		report(id.String(), gen)
	}
	// After an Agent restart the table is rebuilt from what is on disk.
	if err := d.applyNetwork(ctx); err != nil {
		slog.Warn("render the Sandbox network table", "error", err)
	}
}

func (d *containerdDriver) scan(ctx context.Context, report func(string, int64)) {
	for id, gen := range d.r.managedSnapshot() {
		sandboxID, err := uuid.Parse(id)
		if err != nil {
			continue
		}
		if alive, known := d.runtimeAlive(ctx, sandboxID); known && !alive {
			d.r.forget(id)
			report(id, gen)
		}
	}
}

// runtimeAlive reports the task state. known is false when containerd cannot
// be asked; only a Runtime this Node owns counts as alive.
func (d *containerdDriver) runtimeAlive(ctx context.Context, sandboxID uuid.UUID) (bool, bool) {
	c, err := d.client.LoadContainer(ctx, containerName(sandboxID))
	if errdefs.IsNotFound(err) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	labels, err := c.Labels(ctx)
	if err != nil {
		return false, false
	}
	running, err := taskRunning(ctx, c)
	if err != nil {
		return false, false
	}
	return running && containerOwnedBy(labels, d.r.nodeID), true
}

// watchEvents reports a Sandbox whose init process exited without a Stop
// (runtime.md §9); the periodic scan is the fallback when the stream breaks.
func (d *containerdDriver) watchEvents(ctx context.Context, report func(string, int64)) {
	for ctx.Err() == nil {
		events, errs := d.client.Subscribe(ctx,
			`topic=="/tasks/exit",namespace=="`+containerdNamespace+`"`)
	stream:
		for {
			select {
			case <-ctx.Done():
				return
			case <-errs:
				break stream
			case envelope := <-events:
				decoded, err := typeurl.UnmarshalAny(envelope.Event)
				if err != nil {
					continue
				}
				exit, ok := decoded.(*eventtypes.TaskExit)
				if !ok || exit.ID != exit.ContainerID {
					continue // an exec ended, not the Runtime
				}
				c, err := d.client.LoadContainer(ctx, exit.ContainerID)
				if err != nil {
					continue // removed by Stop
				}
				labels, err := c.Labels(ctx)
				if err != nil || !containerOwnedBy(labels, d.r.nodeID) {
					continue
				}
				gen, err := strconv.ParseInt(labels[labelGeneration], 10, 64)
				if err != nil {
					continue
				}
				id := labels[labelSandboxID]
				if gen, tracked := d.r.takeIf(id, gen); tracked {
					report(id, gen)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}
