//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

type linkedSandbox struct {
	Address string `json:"address"`
	NodeID  string `json:"node_id"`
	Links   *struct {
		From  []string `json:"from"`
		Ports []int32  `json:"ports"`
	} `json:"links"`
}

// TestSandboxLinks is #192's single-Node scenario (ADR-0027): a linked source
// in Egress Mode none reaches the target's linked port and nothing else, an
// unlinked open Sandbox does not reach it, and after a restart the target's
// address changes and the Link follows it.
func TestSandboxLinks(t *testing.T) {
	if composeArgs() == nil || os.Getenv("ORRERY_CONFIG_DIR") == "" {
		t.Skip("needs the Compose stack and its config.yaml (scripts/e2e.sh)")
	}
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	node := startAgentNode(t, admin, fmt.Sprintf("e2e-links-%d", suffix))
	if got := capabilityValues(node)["sandbox_links"]; got != "enforced" {
		t.Fatalf("sandbox_links = %q, want enforced (capabilities %v)", got, node.Capabilities)
	}
	sandbox := func(name string, extra map[string]any) string {
		body := map[string]any{"name": fmt.Sprintf("%s-%d", name, suffix), "cpu": 0.5, "memory": 64 << 20, "idle_stop": 0}
		for k, v := range extra {
			body[k] = v
		}
		id := createReadySandboxWith(t, admin, body)
		t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/sandboxes/"+id, admin, nil) })
		return id
	}

	source := sandbox("links-source", map[string]any{"egress": map[string]any{"mode": "none"}})
	open := sandbox("links-open", nil)
	target := sandbox("links-target", map[string]any{"links": map[string]any{"from": []string{source}, "ports": []int{28001}}})
	sb := getLinkedSandbox(t, admin, target)
	if sb.Links == nil || !slices.Equal(sb.Links.From, []string{source}) || !slices.Equal(sb.Links.Ports, []int32{28001}) {
		t.Fatalf("target links = %+v, want from [%s] ports [28001]", sb.Links, source)
	}
	if got := getLinkedSandbox(t, admin, open); got.Links != nil {
		t.Fatalf("a Sandbox without links returns links %+v", got.Links)
	}
	listen(t, admin, target)
	addr := waitSandboxAddress(t, admin, target)
	if eth0 := sandboxAddress(t, admin, target); eth0 != addr {
		t.Fatalf("address = %q, eth0 has %q", addr, eth0)
	}

	// The positive check first: until the Link reaches the Node, the target
	// has no inbound chain, so the negative checks would pass vacuously.
	assertReachSoon(t, admin, source, addr, 28001)
	assertReach(t, admin, source, addr, 28002, false)
	assertReach(t, admin, open, addr, 28001, false)

	// The address is cleared when stopped and changes on every start; the
	// Link follows it (api.md §3).
	restartSandbox(t, admin, target)
	listen(t, admin, target)
	moved := waitSandboxAddress(t, admin, target)
	if moved == addr {
		t.Fatalf("address after restart = %q, unchanged", moved)
	}
	assertReachSoon(t, admin, source, moved, 28001)
	assertReach(t, admin, open, moved, 28001, false)

	// So does a restarted source's new address.
	restartSandbox(t, admin, source)
	assertReachSoon(t, admin, source, moved, 28001)
	assertReach(t, admin, source, moved, 28002, false)

	t.Run("validation", func(t *testing.T) {
		create := func(links map[string]any, wantStatus int, wantCode string) {
			t.Helper()
			status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
				"cpu": 0.5, "memory": 64 << 20, "links": links,
			})
			if status != wantStatus {
				t.Fatalf("create with links %v: %d %s, want %d", links, status, data, wantStatus)
			}
			requireProblemCode(t, data, wantCode)
		}
		create(map[string]any{"from": []string{}}, http.StatusBadRequest, "invalid_request")
		create(map[string]any{"from": []string{"00000000-0000-4000-8000-000000000000"}}, http.StatusBadRequest, "invalid_request")
		create(map[string]any{"from": []string{source}, "ports": []int{0}}, http.StatusBadRequest, "invalid_request")
		create(map[string]any{"from": []string{foreignSandbox(t, admin, suffix)}}, http.StatusBadRequest, "invalid_request")

		// No Local Node is enrolled, so this stays pending until admission
		// times out, but it exists and is not deleted.
		status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
			"runtime": "local", "cpu": 0.5, "memory": 64 << 20,
		})
		if status != http.StatusCreated {
			t.Fatalf("create a Local Sandbox: %d %s", status, data)
		}
		var local sandboxView
		mustJSON(t, data, &local)
		t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/sandboxes/"+local.ID, admin, nil) })
		create(map[string]any{"from": []string{local.ID}}, http.StatusUnprocessableEntity, "unsupported")
	})
}

// foreignSandbox creates a Sandbox owned by a new User; to the admin it is the
// same as a missing one.
func foreignSandbox(t *testing.T, admin string, suffix int64) string {
	t.Helper()
	username, password := fmt.Sprintf("links-other-%d", suffix), "links-other-password-123"
	newCLIRunner(t, admin).run(t, password+"\n", "user", "create",
		"--name", username, "--login-type", "password", "--password-stdin")
	token, status, data := login(t, username, password, "links", "user")
	if status != http.StatusCreated {
		t.Fatalf("login %s: %d %s", username, status, data)
	}
	status, data = call(t, http.MethodGet, "/api/v1/users/me", token, nil)
	var me struct {
		ID string `json:"id"`
	}
	mustJSON(t, data, &me)
	setUserSandboxQuota(t, admin, me.ID, 1)
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes", token, map[string]any{"cpu": 0.5, "memory": 64 << 20})
	if status != http.StatusCreated {
		t.Fatalf("create the other User's Sandbox: %d %s", status, data)
	}
	var sb sandboxView
	mustJSON(t, data, &sb)
	t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/sandboxes/"+sb.ID, token, nil) })
	return sb.ID
}

// TestSandboxLinksAcrossNodes is #192's cross-Node scenario (runtime.md §15):
// this machine's Agent has no --mesh-endpoint and dials out; the remote one
// has one. Two Agents on one host would collide on orrery0 and orrery-wg, so
// it needs a second host:
//
//	ORRERY_E2E_GPU_SSH=user@host ORRERY_E2E_MESH_ENDPOINT=host:51820 make e2e -run SandboxLinksAcrossNodes
//
// The SSH login must reach root (directly or with sudo -n), and the remote
// host needs containerd, the CNI plugins and kernel WireGuard. The endpoint
// defaults to the SSH host on port 51820, which must be reachable over UDP
// from this machine.
func TestSandboxLinksAcrossNodes(t *testing.T) {
	if os.Getenv("ORRERY_E2E_GPU_SSH") == "" {
		t.Skip("ORRERY_E2E_GPU_SSH is not set; Sandbox Links across Nodes need a second host " +
			"(two Agents on one host collide on the bridge and orrery-wg)")
	}
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	endpoint := os.Getenv("ORRERY_E2E_MESH_ENDPOINT")
	if endpoint == "" {
		target := os.Getenv("ORRERY_E2E_GPU_SSH")
		endpoint = target[strings.LastIndex(target, "@")+1:] + ":51820"
	}

	// Enroll this machine's Node first so it is the older one, which placement
	// prefers for the unbound sources.
	localName := fmt.Sprintf("e2e-mesh-local-%d", suffix)
	state, dataRoot := enrollAgent(t, admin, localName)
	remote := startRemoteHost(t, "e2e-mesh-remote", func(*localHost) []string {
		return []string{"--mesh-endpoint", endpoint, "--sandbox-subnet", "10.214.0.0/16"}
	})
	waitForNodeReady(t, admin, remote.nodeName)
	far := findNode(t, admin, remote.nodeName)
	if got := capabilityValues(far)["sandbox_links"]; got != "enforced" {
		t.Fatalf("remote sandbox_links = %q, want enforced", got)
	}
	// Workspaces created while only the remote Node is connected bind to it.
	targetWS := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)
	otherWS := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)

	startAgentWith(t, state, dataRoot)
	waitForNodeReady(t, admin, localName)
	near := findNode(t, admin, localName)

	create := func(name string, body map[string]any, wantNode string) string {
		body["name"] = fmt.Sprintf("%s-%d", name, suffix)
		body["cpu"], body["memory"], body["idle_stop"] = 0.5, 128<<20, 0
		status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, body)
		if status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, status, data)
		}
		var sb sandboxView
		mustJSON(t, data, &sb)
		t.Cleanup(func() { _, _ = call(t, http.MethodDelete, "/api/v1/sandboxes/"+sb.ID, admin, nil) })
		// A cross-Node target waits until the Mesh path is live.
		ready := waitForSandboxState(t, admin, sb.ID, "ready", 180*time.Second)
		if ready.NodeID == nil || *ready.NodeID != wantNode {
			t.Fatalf("%s node_id = %v, want %s", name, ready.NodeID, wantNode)
		}
		return sb.ID
	}
	source := create("mesh-source", map[string]any{"egress": map[string]any{"mode": "none"}}, near.ID)
	unlinked := create("mesh-unlinked", map[string]any{}, near.ID)
	target := create("mesh-target", map[string]any{
		"links":     map[string]any{"from": []string{source}},
		"workspace": map[string]string{"id": targetWS.ID, "mode": "rw"},
	}, far.ID)
	other := create("mesh-other", map[string]any{
		"workspace": map[string]string{"id": otherWS.ID, "mode": "rw"},
	}, far.ID)

	listen(t, admin, target)
	addr := waitSandboxAddress(t, admin, target)
	assertReachSoon(t, admin, source, addr, 28001)
	assertReach(t, admin, unlinked, addr, 28001, false)
	// The mesh carries nothing between unlinked Sandboxes, in either
	// direction: the source has no inbound chain of its own, so only the
	// orrery_mesh filter on the remote Node stops this.
	listen(t, admin, unlinked)
	assertReach(t, admin, other, waitSandboxAddress(t, admin, unlinked), 28001, false)

	// A multi-MB transfer exercises full-size segments through orrery-wg's
	// smaller MTU (the MSS clamp).
	runExecScript(t, admin, target,
		"mkdir -p /tmp/www && head -c 8388608 /dev/urandom > /tmp/www/blob && sha256sum /tmp/www/blob | cut -d' ' -f1 > /tmp/blob.sum")
	createExec(t, admin, target, map[string]any{"argv": []string{"busybox", "httpd", "-f", "-p", "28003", "-h", "/tmp/www"}})
	want := strings.TrimSpace(execOutput(t, admin, target, "cat /tmp/blob.sum"))
	got := strings.TrimSpace(execOutput(t, admin, source,
		fmt.Sprintf("for i in $(seq 1 20); do nc -z -w 1 %s 28003 && break; sleep 0.5; done; wget -q -T 60 -O - http://%s:28003/blob | sha256sum | cut -d' ' -f1", addr, addr)))
	if got != want {
		t.Fatalf("8 MiB over the Mesh: sha256 %q, want %q", got, want)
	}
}

func getLinkedSandbox(t *testing.T, admin, id string) linkedSandbox {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/sandboxes/"+id, admin, nil)
	if status != http.StatusOK {
		t.Fatalf("get Sandbox %s: %d %s", id, status, data)
	}
	var sb linkedSandbox
	mustJSON(t, data, &sb)
	return sb
}

// waitSandboxAddress returns the API's address for a running Sandbox; it is
// recorded from the Agent's running report, so it can trail ready briefly.
func waitSandboxAddress(t *testing.T, admin, id string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if sb := getLinkedSandbox(t, admin, id); sb.Address != "" {
			return sb.Address
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox %s has no address", id)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// assertReachSoon waits for a Link to take effect: SandboxLinks is pushed on
// the reconcile tick after an address changes.
func assertReachSoon(t *testing.T, admin, from, addr string, port int) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		e := createExec(t, admin, from, map[string]any{"argv": []string{"nc", "-z", "-w", "3", addr, fmt.Sprint(port)}})
		result := waitExecFinished(t, admin, e.ID, 30*time.Second).Result
		if result != nil && result.ExitCode != nil && *result.ExitCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Sandbox %s never reached %s:%d (last result %+v)", from, addr, port, result)
		}
		time.Sleep(time.Second)
	}
}

// execOutput runs a shell script in a Sandbox and returns its stdout.
func execOutput(t *testing.T, admin, id, script string) string {
	t.Helper()
	e := createExec(t, admin, id, map[string]any{"argv": []string{"sh", "-c", script}})
	assertExitedWith(t, waitExecFinished(t, admin, e.ID, 120*time.Second), "exited")
	return execStdout(t, admin, e.ID)
}
