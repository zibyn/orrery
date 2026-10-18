//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zibyn/stars-sandbox/client"
)

// sdkClient is the public Go SDK pointed at the running e2e deployment.
func sdkClient(t *testing.T) *client.Client {
	t.Helper()
	return client.New(apiURL(), adminToken(t))
}

// TestSDKParityExecLogStream runs M1 "exec-log-stream" through the client
// package and checks it against the same command run over REST directly.
func TestSDKParityExecLogStream(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)
	ctx := context.Background()
	c := sdkClient(t)

	command := []string{"sh", "-c", `printf 'hello '; printf 'oops' >&2; printf '\377'; exit 7`}

	// The REST path is the reference.
	reference := createExec(t, admin, sandboxID, map[string]any{"argv": command})
	refOut, refErr, _, refResult := followExec(t, admin, reference.ID, 30*time.Second)

	// The same command through the SDK.
	created, err := c.CreateExec(ctx, sandboxID, client.ExecRequest{Argv: command}, "")
	if err != nil {
		t.Fatalf("CreateExec: %v", err)
	}
	var stdout, stderr bytes.Buffer
	final, err := c.FollowOutput(ctx, created.ID, &stdout, &stderr)
	if err != nil {
		t.Fatalf("FollowOutput: %v", err)
	}

	wantOut := append([]byte("hello "), 0xff)
	if !bytes.Equal(stdout.Bytes(), wantOut) {
		t.Fatalf("sdk stdout = %q, want %q", stdout.Bytes(), wantOut)
	}
	if !bytes.Equal(stdout.Bytes(), refOut) {
		t.Fatalf("sdk stdout = %q, REST stdout = %q", stdout.Bytes(), refOut)
	}
	if stderr.String() != "oops" || stderr.String() != string(refErr) {
		t.Fatalf("sdk stderr = %q, REST stderr = %q", stderr.String(), refErr)
	}
	if final.State != "finished" || final.Result == nil || final.Result.Cause != "exited" ||
		final.Result.ExitCode == nil || *final.Result.ExitCode != 7 {
		t.Fatalf("sdk result = %+v", final)
	}
	if refResult.Cause != final.Result.Cause || refResult.ExitCode == nil || *refResult.ExitCode != 7 {
		t.Fatalf("REST result = %+v, sdk result = %+v", refResult, final.Result)
	}

	// The SDK's Exec read and output page agree with REST.
	direct := getExec(t, admin, created.ID)
	if direct.State != final.State || direct.Result == nil || direct.Result.Cause != final.Result.Cause {
		t.Fatalf("sdk GetExec = %+v, REST = %+v", final, direct)
	}
	sdkPage, err := c.ExecOutput(ctx, created.ID, 0)
	if err != nil {
		t.Fatalf("ExecOutput: %v", err)
	}
	status, data := call(t, http.MethodGet, "/api/v1/execs/"+created.ID+"/output?after=0", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("output page: %d %s", status, data)
	}
	var restPage []execFrame
	mustJSON(t, data, &restPage)
	if len(sdkPage) != len(restPage) {
		t.Fatalf("sdk page has %d frames, REST has %d", len(sdkPage), len(restPage))
	}
	for i := range sdkPage {
		f := sdkPage[i]
		r := restPage[i]
		if f.Seq != r.Seq || f.Stream != r.Stream || f.Data != r.Data || f.Encoding != r.Encoding {
			t.Fatalf("frame %d: sdk %+v, REST %+v", i, f, r)
		}
	}
}

// TestSDKParityFilesAPI runs M1 "files-api" through the client package and
// compares each result with the REST calls the scenario already uses.
func TestSDKParityFilesAPI(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	sandboxID := readyExecSandbox(t, admin, 128<<20)
	ctx := context.Background()
	c := sdkClient(t)
	files := "/api/v1/sandboxes/" + sandboxID + "/files"

	restGet := func(path string, header map[string]string) (int, []byte) {
		t.Helper()
		return fileCallHeader(t, admin, http.MethodGet, files+"/content?path="+url.QueryEscape(path), nil, header)
	}

	// Upload through the SDK, read back through REST and the SDK.
	content := []byte("hello workspace\n")
	if err := c.WriteFile(ctx, sandboxID, "src/hello.txt", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("sdk WriteFile: %v", err)
	}
	status, got := restGet("src/hello.txt", nil)
	if status != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("REST download = %d %q", status, got)
	}
	var downloaded bytes.Buffer
	if err := c.ReadFile(ctx, sandboxID, "src/hello.txt", &downloaded, ""); err != nil {
		t.Fatalf("sdk ReadFile: %v", err)
	}
	if !bytes.Equal(downloaded.Bytes(), content) {
		t.Fatalf("sdk download = %q", downloaded.Bytes())
	}
	var ranged bytes.Buffer
	if err := c.ReadFile(ctx, sandboxID, "src/hello.txt", &ranged, "bytes=6-14"); err != nil {
		t.Fatalf("sdk ranged ReadFile: %v", err)
	}
	if ranged.String() != "workspace" {
		t.Fatalf("sdk range = %q", ranged.String())
	}

	// List and stat through the SDK agree with REST.
	list, err := c.ListFiles(ctx, sandboxID, "src")
	if err != nil {
		t.Fatalf("sdk ListFiles: %v", err)
	}
	status, data := call(t, http.MethodGet, files+"?path=src", admin, nil)
	var restList struct {
		Items []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int64  `json:"size"`
		} `json:"items"`
	}
	mustJSON(t, data, &restList)
	if status != http.StatusOK || len(list.Items) != len(restList.Items) {
		t.Fatalf("sdk list = %+v, REST list = %+v", list, restList)
	}
	for i, item := range list.Items {
		if item.Path != restList.Items[i].Path || item.Type != restList.Items[i].Type ||
			item.Size != restList.Items[i].Size {
			t.Fatalf("entry %d: sdk %+v, REST %+v", i, item, restList.Items[i])
		}
	}
	entry, err := c.StatFile(ctx, sandboxID, "src/hello.txt")
	if err != nil {
		t.Fatalf("sdk StatFile: %v", err)
	}
	status, data = call(t, http.MethodGet, files+"/stat?path=src/hello.txt", admin, nil)
	if status != http.StatusOK || entry.Type != "file" || entry.Size != int64(len(content)) ||
		!strings.Contains(string(data), `"type":"file"`) {
		t.Fatalf("sdk stat = %+v, REST = %s", entry, data)
	}

	// mkdir, move and delete.
	if err := c.Mkdir(ctx, sandboxID, "empty"); err != nil {
		t.Fatalf("sdk Mkdir: %v", err)
	}
	status, data = call(t, http.MethodGet, files+"/stat?path=empty", admin, nil)
	if status != http.StatusOK || !strings.Contains(string(data), `"type":"dir"`) {
		t.Fatalf("REST stat of mkdir = %d %s", status, data)
	}
	if err := c.MoveFile(ctx, sandboxID, "src/hello.txt", "src/moved.txt"); err != nil {
		t.Fatalf("sdk MoveFile: %v", err)
	}
	if status, _ := restGet("src/moved.txt", nil); status != http.StatusOK {
		t.Fatalf("moved file = %d", status)
	}
	if err := c.DeleteFile(ctx, sandboxID, "src/moved.txt"); err != nil {
		t.Fatalf("sdk DeleteFile: %v", err)
	}
	if status, _ := restGet("src/moved.txt", nil); status != http.StatusNotFound {
		t.Fatalf("deleted file = %d, want 404", status)
	}

	// Escaping paths are refused, never rewritten.
	for _, path := range []string{"../escape.txt", "/etc/passwd", "src/../../escape.txt"} {
		var problem *client.Error
		err := c.WriteFile(ctx, sandboxID, path, strings.NewReader("x"), 1)
		if !errors.As(err, &problem) || problem.Status != http.StatusBadRequest || problem.Code != "invalid_request" {
			t.Fatalf("sdk write %q = %v, want 400 invalid_request", path, err)
		}
		err = c.ReadFile(ctx, sandboxID, path, &bytes.Buffer{}, "")
		if !errors.As(err, &problem) || problem.Status != http.StatusBadRequest {
			t.Fatalf("sdk read %q = %v, want 400", path, err)
		}
	}

	// An escaping symlink is listed but never followed.
	e := createExec(t, admin, sandboxID, map[string]any{"argv": []string{"ln", "-s", "/etc", "escape"}})
	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 10*time.Second), "exited")
	var problem *client.Error
	if err := c.ReadFile(ctx, sandboxID, "escape/passwd", &bytes.Buffer{}, ""); !errors.As(err, &problem) ||
		problem.Status != http.StatusBadRequest {
		t.Fatalf("sdk escaping symlink = %v, want 400", err)
	}
	link, err := c.StatFile(ctx, sandboxID, "escape")
	if err != nil || link.Type != "symlink" {
		t.Fatalf("sdk symlink stat = %+v, %v", link, err)
	}

	// Archive download and upload preserve the subtree.
	if err := c.WriteFile(ctx, sandboxID, "tree/a.txt", strings.NewReader("alpha"), 5); err != nil {
		t.Fatalf("sdk WriteFile: %v", err)
	}
	var archive bytes.Buffer
	if err := c.ReadArchive(ctx, sandboxID, "tree", &archive); err != nil {
		t.Fatalf("sdk ReadArchive: %v", err)
	}
	if names := tarNames(t, archive.Bytes()); len(names) < 2 {
		t.Fatalf("sdk archive = %v", names)
	}
	if err := c.WriteArchive(ctx, sandboxID, "unpacked", bytes.NewReader(archive.Bytes()), int64(archive.Len())); err != nil {
		t.Fatalf("sdk WriteArchive: %v", err)
	}
	status, got = restGet("unpacked/tree/a.txt", nil)
	if status != http.StatusOK || string(got) != "alpha" {
		t.Fatalf("unpacked = %d %q", status, got)
	}

	// A Sandbox that is not ready answers conflict.
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes/"+sandboxID+"/stop", admin, nil)
	if status != http.StatusAccepted {
		t.Fatalf("stop = %d %s", status, data)
	}
	call(t, http.MethodGet, "/api/v1/sandboxes/"+sandboxID+"?wait_for=stopped", admin, nil)
	if err := c.WriteFile(ctx, sandboxID, "stopped.txt", strings.NewReader("x"), 1); !errors.As(err, &problem) ||
		problem.Status != http.StatusConflict || problem.Code != "conflict" {
		t.Fatalf("sdk write to stopped Sandbox = %v, want 409 conflict", err)
	}
}

// TestSDKParityGPUSandboxReady runs M1 "gpu-sandbox-ready" through the client
// package on a real GPU host and compares it with the REST view.
func TestSDKParityGPUSandboxReady(t *testing.T) {
	admin := adminToken(t)
	host := startGPUHost(t, "e2e-sdk-gpu")
	waitForNodeReady(t, admin, host.nodeName)
	node := findNode(t, admin, host.nodeName)
	cards := requireGPUs(t, node)
	setGPUQuota(t, admin, int32(cards), int32(cards)+2)
	ctx := context.Background()
	c := sdkClient(t)

	body := gpuSandboxBody(t, node, 1)
	cpu := body["cpu"].(float64)
	memory := int64(body["memory"].(int))
	idleStop := int32(body["idle_stop"].(int))
	snapshot := body["snapshot"].(string)
	req := client.CreateSandboxRequest{
		Name:     strPtr(fmt.Sprintf("sdk-gpu-%d", time.Now().UnixNano())),
		CPU:      &cpu,
		Memory:   &memory,
		IdleStop: &idleStop,
		Snapshot: &snapshot,
		GPU:      &client.SandboxGPU{Count: 1},
	}
	created, err := c.CreateSandbox(ctx, req, "")
	if err != nil {
		t.Fatalf("sdk CreateSandbox: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteSandbox(context.Background(), created.ID) })
	ready, err := c.GetSandbox(ctx, created.ID, "ready")
	if err != nil {
		t.Fatalf("sdk wait_for=ready: %v", err)
	}
	if ready.State != "ready" || ready.GPU == nil || ready.GPU.Count != 1 {
		t.Fatalf("sdk Sandbox = %+v", ready)
	}

	// The REST view of the same Sandbox agrees.
	direct := getSandbox(t, admin, created.ID)
	if direct.State != ready.State || direct.CPU != ready.CPU || direct.Memory != ready.Memory ||
		direct.GPU == nil || direct.GPU.Count != ready.GPU.Count {
		t.Fatalf("sdk Sandbox = %+v, REST = %+v", ready, direct)
	}

	// The container sees exactly the allocated card.
	assigned := psqlCount(t, "SELECT gpu_uuid FROM gpu_allocations WHERE sandbox_id = '"+created.ID+"'")
	if assigned == "" {
		t.Fatalf("no GPU allocation for Sandbox %s", created.ID)
	}
	seen := remoteGPUUUIDs(t, host, created.ID)
	if len(seen) != 1 || seen[0] != assigned {
		t.Fatalf("the Sandbox sees %v, want the allocated card %s", seen, assigned)
	}

	// The SDK capacity view matches the REST one.
	capacity, err := c.Capacity(ctx)
	if err != nil {
		t.Fatalf("sdk Capacity: %v", err)
	}
	var allocated, total int
	for _, item := range capacity.Items {
		if item.NodeID != node.ID {
			continue
		}
		for _, gpu := range item.GPUs {
			allocated += gpu.Allocated
			total += gpu.Total
		}
	}
	restAllocated, restTotal := gpuCapacity(t, admin, node.ID)
	if allocated != restAllocated || total != restTotal || allocated != 1 || total != cards {
		t.Fatalf("sdk capacity = %d/%d, REST = %d/%d, want 1/%d",
			allocated, total, restAllocated, restTotal, cards)
	}

	// Deleting through the SDK releases the card.
	if _, err := c.DeleteSandbox(ctx, created.ID); err != nil {
		t.Fatalf("sdk DeleteSandbox: %v", err)
	}
	waitForGPUCapacity(t, admin, node.ID, 0)
	if used := myQuota(t, admin).GPUs.Used; used != 0 {
		t.Fatalf("GPU quota used after delete = %d, want 0", used)
	}
}

func strPtr(s string) *string { return &s }
