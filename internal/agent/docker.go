package agent

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zibyn/stars-sandbox/internal/nodeproto"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
)

const (
	probeImage      = "alpine:3.22"
	probeNanoCPUs   = int64(500_000_000) // 0.5 CPU
	probeMemory     = int64(64 << 20)
	probePidsLimit  = int64(64)
	probeWorkspace  = "/workspace"
	probeSupervisor = "/.stars/bin/supervisor"
)

// dockerProbe runs one probe container and turns the readback into Runtime
// Capabilities (runtime.md §9). It needs no Docker client dependency: the Engine
// API over the socket is a handful of JSON requests.
func dockerProbe(ctx context.Context, socket string) (nodeproto.CapabilityReport, error) {
	d := &dockerClient{http: unixHTTPClient(socket)}
	if err := d.negotiate(ctx); err != nil {
		return nodeproto.CapabilityReport{}, err
	}
	if err := d.ensureImage(ctx); err != nil {
		return nodeproto.CapabilityReport{}, err
	}

	dir, err := os.MkdirTemp("", "stars-probe-*")
	if err != nil {
		return nodeproto.CapabilityReport{}, err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // best effort
	binDir := filepath.Join(dir, "bin")
	workspace := filepath.Join(dir, "workspace")
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

	// The host inventory decides what the probe asks CDI for; a CPU-only Node
	// sends no DeviceRequest at all.
	gpus := enumerateGPUs(ctx)
	uuids := gpuUUIDs(gpus)
	// Each capability is read back on its own (runtime.md §9), so a rejected
	// limit must not hide a working CDI attachment and vice versa: try the
	// bounded GPU probe, the bounded plain probe, then the same two unbounded.
	attempts := []struct{ limits, gpus bool }{{true, false}, {false, false}}
	if len(uuids) > 0 {
		attempts = []struct{ limits, gpus bool }{
			{true, true}, {true, false}, {false, true}, {false, false},
		}
	}
	var result supervisor.Result
	var limitsEnforced, gpuVisible bool
	var probeErr error
	for _, attempt := range attempts {
		var requested []string
		if attempt.gpus {
			requested = uuids
		}
		result, limitsEnforced, probeErr = d.probe(ctx, binDir, workspace, attempt.limits, requested)
		if probeErr == nil {
			gpuVisible = attempt.gpus && numberedDeviceCount(result.NvidiaDevices) == len(uuids)
			break
		}
	}
	if probeErr != nil {
		return nodeproto.CapabilityReport{}, probeErr
	}
	return capabilities(result, limitsEnforced, gpuVisible), nil
}

func capabilities(r supervisor.Result, limitsEnforced, gpuVisible bool) nodeproto.CapabilityReport {
	value := func(ok bool) string {
		if ok {
			return nodeproto.ValueEnforced
		}
		return nodeproto.ValueUnsupported
	}
	pidNS, mntNS := supervisor.NamespaceID("/proc/self/ns/pid"), supervisor.NamespaceID("/proc/self/ns/mnt")
	isolated := r.PIDNamespace != "" && r.PIDNamespace != pidNS &&
		r.MountNamespace != "" && r.MountNamespace != mntNS
	return nodeproto.CapabilityReport{
		Runtime: "docker",
		Entries: []nodeproto.CapabilityEntry{
			{Name: nodeproto.CapProcessMountNamespace, Value: value(isolated)},
			{Name: nodeproto.CapCPUMemoryPIDLimits, Value: value(limitsEnforced)},
			{Name: nodeproto.CapWorkspaceRWMount, Value: value(r.WorkspaceWritable)},
			{Name: nodeproto.CapExecSupervisor, Value: nodeproto.ValueEnforced},
			// Whole-card visibility is enforced by CDI; GPU memory limits stay
			// unsupported (ADR-0009).
			{Name: nodeproto.CapGPUDeviceVisibility, Value: value(gpuVisible)},
			{Name: nodeproto.CapGPUMemoryLimit, Value: nodeproto.ValueUnsupported},
			// Disk quota and arbitrary mounts are separate tickets; report them
			// honestly instead of guessing.
			{Name: nodeproto.CapDiskCapacityQuota, Value: nodeproto.ValueUnsupported},
			{Name: nodeproto.CapArbitraryMounts, Value: nodeproto.ValueUnsupported},
		},
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // read-only
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck // report the copy error instead
		return err
	}
	return out.Close()
}

type dockerClient struct {
	http    *http.Client
	version string
}

// dockerHealthy is the cheap heartbeat check: the daemon answers /version.
func dockerHealthy(ctx context.Context, socket string) bool {
	d := &dockerClient{http: unixHTTPClient(socket)}
	return d.negotiate(ctx) == nil
}

func unixHTTPClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var dialer net.Dialer
				return dialer.DialContext(ctx, "unix", socket)
			},
		},
	}
}

func (d *dockerClient) negotiate(ctx context.Context) error {
	var info struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := d.do(ctx, http.MethodGet, "/version", nil, &info); err != nil {
		return fmt.Errorf("docker version: %w", err)
	}
	if info.APIVersion == "" {
		return errors.New("docker version: empty ApiVersion")
	}
	d.version = info.APIVersion
	return nil
}

func (d *dockerClient) api(path string) string { return "/v" + d.version + path }

func (d *dockerClient) ensureImage(ctx context.Context) error {
	return d.ensureImageRef(ctx, probeImage, "")
}

// probe creates, starts and reads back one container. limitsEnforced is true
// only when the daemon accepted the limits and every readback matched.
func (d *dockerClient) probe(ctx context.Context, binDir, workspace string, withLimits bool, gpuUUIDs []string) (supervisor.Result, bool, error) {
	name := "stars-sandbox-probe-" + uuid.NewString()
	hostConfig := map[string]any{
		"CgroupnsMode": "private",
		"NetworkMode":  "none",
		"Binds": []string{
			workspace + ":" + probeWorkspace + ":rw",
			binDir + ":/.stars/bin:ro",
		},
	}
	if withLimits {
		hostConfig["NanoCpus"] = probeNanoCPUs
		hostConfig["Memory"] = probeMemory
		hostConfig["PidsLimit"] = probePidsLimit
	}
	if requests := cdiDeviceRequest(gpuUUIDs); requests != nil {
		hostConfig["DeviceRequests"] = requests
	}
	create := map[string]any{
		"Image":      probeImage,
		"Entrypoint": []string{probeSupervisor},
		"Cmd":        []string{"probe"},
		"WorkingDir": probeWorkspace,
		"HostConfig": hostConfig,
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodPost, d.api("/containers/create")+"?name="+name, create, &created); err != nil {
		return supervisor.Result{}, false, fmt.Errorf("create probe container: %w", err)
	}
	defer func() {
		// The probe container is disposable whether or not it started.
		resp, err := d.request(context.WithoutCancel(ctx), http.MethodDelete,
			d.api("/containers/"+created.ID)+"?force=true&v=true", nil)
		if err == nil {
			resp.Body.Close()
		}
	}()

	if err := d.do(ctx, http.MethodPost, d.api("/containers/"+created.ID+"/start"), nil, nil); err != nil {
		return supervisor.Result{}, false, fmt.Errorf("start probe container: %w", err)
	}
	var waited struct {
		StatusCode int64 `json:"StatusCode"`
	}
	if err := d.do(ctx, http.MethodPost, d.api("/containers/"+created.ID+"/wait?condition=not-running"), nil, &waited); err != nil {
		return supervisor.Result{}, false, fmt.Errorf("wait for probe: %w", err)
	}
	resp, err := d.request(ctx, http.MethodGet, d.api("/containers/"+created.ID+"/logs?stdout=true&stderr=true"), nil)
	if err != nil {
		return supervisor.Result{}, false, fmt.Errorf("read probe output: %w", err)
	}
	defer resp.Body.Close()
	logs, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return supervisor.Result{}, false, err
	}
	stdout := demux(logs)
	if waited.StatusCode != 0 {
		return supervisor.Result{}, false, fmt.Errorf("probe exited %d: %s", waited.StatusCode, strings.TrimSpace(string(stdout)))
	}
	var result supervisor.Result
	if err := json.Unmarshal(stdout, &result); err != nil {
		return supervisor.Result{}, false, fmt.Errorf("decode probe output: %w", err)
	}
	enforced := withLimits &&
		result.CPUMax == "50000 100000" &&
		result.MemoryMax == fmt.Sprint(probeMemory) &&
		result.PidsMax == fmt.Sprint(probePidsLimit)
	return result, enforced, nil
}

// demux strips Docker's 8-byte stream headers (stdout/stderr) from logs.
func demux(raw []byte) []byte {
	var out bytes.Buffer
	for len(raw) >= 8 {
		size := int(binary.BigEndian.Uint32(raw[4:8]))
		raw = raw[8:]
		if size > len(raw) {
			size = len(raw)
		}
		out.Write(raw[:size])
		raw = raw[size:]
	}
	if out.Len() == 0 {
		return raw
	}
	return out.Bytes()
}

func (d *dockerClient) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := d.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck // drain for reuse
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (d *dockerClient) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	return d.requestHeaders(ctx, method, path, body, nil)
}

func (d *dockerClient) requestHeaders(ctx context.Context, method, path string, body any, headers map[string]string) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	return d.http.Do(req)
}
