//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/stars-sandbox/gen/node/v1"
	"github.com/zibyn/stars-sandbox/internal/nodeproto"
)

// startAgentNode enrolls and starts one Agent, waits for its Node to become
// ready and returns the Node. Cleanup kills the process with the test.
func startAgentNode(t *testing.T, admin, name string, extra ...string) nodeView {
	t.Helper()
	state, dataRoot := enrollAgent(t, admin, name)
	startAgentWith(t, state, dataRoot, extra...)
	waitForNodeReady(t, admin, name)
	return findNode(t, admin, name)
}

func getNode(t *testing.T, admin, id string) nodeView {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/nodes/"+id, admin, nil)
	if status != http.StatusOK {
		t.Fatalf("get Node: %d %s", status, data)
	}
	var node nodeView
	mustJSON(t, data, &node)
	return node
}

// waitForNodeHealth polls by name until the Node reports the wanted derived
// status and heartbeat health.
func waitForNodeHealth(t *testing.T, admin, name, wantStatus, wantHealth string) nodeView {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		node := findNode(t, admin, name)
		if node.Status == wantStatus && node.RuntimeHealth != nil && *node.RuntimeHealth == wantHealth {
			return node
		}
		if time.Now().After(deadline) {
			t.Fatalf("Node %q stayed status %q, runtime_health %v; want %q, %q",
				name, node.Status, node.RuntimeHealth, wantStatus, wantHealth)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestMultiNodePlacement is #93's "multi-node-placement" scenario: with two
// Nodes on one Docker daemon, a new persistent Workspace lands on a feasible
// Node, and its Sandbox is admitted only on the Node the Workspace is bound to
// (ADR-0015 locality). The policy would prefer the older Node, so the two
// Agents are brought up out of order to make a broken pin visible.
func TestMultiNodePlacement(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	firstName := fmt.Sprintf("e2e-multi-first-%d", suffix)
	secondName := fmt.Sprintf("e2e-multi-second-%d", suffix)

	// Enroll both so created_at is fixed by enrollment order: the first Node is
	// the older one, which the placement policy prefers on a tie.
	firstState, firstRoot := enrollAgent(t, admin, firstName)
	secondState, secondRoot := enrollAgent(t, admin, secondName)

	// Only the second (younger) Node is connected when the Workspace is placed.
	startAgentWith(t, secondState, secondRoot, "--runtime", "docker")
	waitForNodeReady(t, admin, secondName)
	second := findNode(t, admin, secondName)

	ws := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)
	if ws.NodeID == nil || *ws.NodeID != second.ID {
		t.Fatalf("Workspace node_id = %v, want the only connected Node %s", ws.NodeID, second.ID)
	}

	// The older Node joins now. Without the Workspace locality filter the
	// Scheduler would prefer it; the pin must keep the Sandbox on the second.
	startAgentWith(t, firstState, firstRoot, "--runtime", "docker")
	waitForNodeReady(t, admin, firstName)
	first := findNode(t, admin, firstName)

	status, data := createSandboxWithWorkspace(t, admin, fmt.Sprintf("multi-%d", suffix), ws.ID, "rw", 0)
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox: %d %s", status, data)
	}
	var created sandboxView
	mustJSON(t, data, &created)
	sb := waitForSandboxReady(t, admin, created.ID)
	if sb.NodeID == nil || *sb.NodeID != second.ID {
		t.Fatalf("Sandbox node_id = %v, want the bound Node %s (the other Node is %s)",
			sb.NodeID, second.ID, first.ID)
	}
	// Every Runtime instance carries its Node's ownership label (runtime.md §8).
	if got := sandboxContainerLabel(t, sb.ID, "stars.node"); got != second.ID {
		t.Fatalf("container stars.node = %q, want %q", got, second.ID)
	}

	deleteSandbox(t, admin, sb.ID)
	if status, data := call(t, http.MethodDelete, "/api/v1/workspaces/"+ws.ID, admin, nil); status != http.StatusAccepted {
		t.Fatalf("delete Workspace: %d %s", status, data)
	}
	deadline := time.Now().Add(60 * time.Second)
	for getWorkspace(t, admin, ws.ID).State != "deleted" {
		if time.Now().After(deadline) {
			t.Fatalf("Workspace %s never became deleted", ws.ID)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestUnhealthyRuntime is #93's "unhealthy-runtime" scenario: a Node whose
// Runtime self-check and heartbeat report unhealthy receives no new placement.
// The shared Docker daemon cannot be stopped for one Agent, so the unhealthy
// Agent is given an unreachable socket.
func TestUnhealthyRuntime(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	healthyName := fmt.Sprintf("e2e-healthy-node-%d", suffix)
	unhealthyName := fmt.Sprintf("e2e-unhealthy-node-%d", suffix)

	healthy := startAgentNode(t, admin, healthyName, "--runtime", "docker")

	state, dataRoot := enrollAgent(t, admin, unhealthyName)
	startAgentWith(t, state, dataRoot, "--runtime", "docker",
		"--docker", filepath.Join(t.TempDir(), "unreachable.sock"))
	unhealthy := waitForNodeHealth(t, admin, unhealthyName, "unavailable", "unhealthy")
	if unhealthy.ID == healthy.ID {
		t.Fatalf("the unhealthy Agent reused Node %s", healthy.ID)
	}
	if got := getNode(t, admin, unhealthy.ID); got.RuntimeHealth == nil || *got.RuntimeHealth != "unhealthy" {
		t.Fatalf("Node %s runtime_health = %v, want unhealthy", unhealthy.ID, got.RuntimeHealth)
	}

	ws := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)
	if ws.NodeID == nil || *ws.NodeID != healthy.ID {
		t.Fatalf("Workspace node_id = %v, want the healthy Node %s", ws.NodeID, healthy.ID)
	}
}

// TestVersionGate is #93's "version-gate" scenario (ADR-0014): a newer minor
// and a different major are both refused with incompatible, and the refusal
// does not cost the Agent its identity.
func TestVersionGate(t *testing.T) {
	admin := adminToken(t)
	name := fmt.Sprintf("e2e-version-node-%d", time.Now().UnixNano())
	state, _ := enrollAgent(t, admin, name)
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatal(err)
	}
	var identity struct {
		NodeID    string `json:"node_id"`
		NodeToken string `json:"node_token"`
	}
	mustJSON(t, raw, &identity)

	client, closeConn := nodeClient(t)
	defer closeConn()

	for name, tt := range map[string]struct{ major, minor uint32 }{
		"a newer minor":     {nodeproto.Major, nodeproto.Minor + 1},
		"a different major": {nodeproto.Major + 1, nodeproto.Minor},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rawHello(t, client, identity.NodeID, identity.NodeToken, tt.major, tt.minor)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("Hello error = %v, want FailedPrecondition", err)
			}
			incompatible := false
			for _, detail := range status.Convert(err).Details() {
				if info, ok := detail.(*errdetails.ErrorInfo); ok && info.Reason == "incompatible" {
					incompatible = true
				}
			}
			if !incompatible {
				t.Fatalf("Hello error %v does not carry reason incompatible", err)
			}
		})
	}

	// The same token still connects with a compatible Hello: an incompatible
	// handshake never invalidates the Node identity (ADR-0014).
	ack, err := rawHello(t, client, identity.NodeID, identity.NodeToken, nodeproto.Major, nodeproto.Minor)
	if err != nil {
		t.Fatalf("the identity was lost after an incompatible Hello: %v", err)
	}
	if ack == nil || !ack.Accepted {
		t.Fatalf("HelloAck = %+v, want accepted", ack)
	}
}

func rawHello(t *testing.T, client nodev1.NodeClient, nodeID, token string, major, minor uint32) (*nodev1.HelloAck, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	stream, err := client.Control(ctx)
	if err != nil {
		return nil, err
	}
	if err := stream.Send(&nodev1.AgentMessage{Message: &nodev1.AgentMessage_Hello{
		Hello: &nodev1.Hello{
			NodeId:       nodeID,
			AgentVersion: "e2e-version-gate",
			ProtoMajor:   major,
			ProtoMinor:   minor,
		},
	}}); err != nil {
		return nil, err
	}
	msg, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	return msg.GetHelloAck(), nil
}

func sandboxContainerLabel(t *testing.T, sandboxID, key string) string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f",
		fmt.Sprintf(`{{index .Config.Labels %q}}`, key), "stars-sandbox-"+sandboxID).CombinedOutput()
	if err != nil {
		t.Fatalf("docker inspect label %s: %v\n%s", key, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestRetireNode is #94's "retire-node" scenario (#39, ADR-0014). There is no
// migration or eviction: a Workspace leaves a Node the way a caller moves data
// — archive it, publish the archive to a Resource, restore it into a Workspace
// on another Node. Only a paused Node with no non-terminal Workspace and no
// non-terminal Sandbox can be deleted; the row stays as a tombstone.
func TestRetireNode(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	oldName := fmt.Sprintf("e2e-retire-old-%d", suffix)
	newName := fmt.Sprintf("e2e-retire-new-%d", suffix)

	// The retiring Node is enrolled and started first, so placement prefers it.
	oldState, oldRoot := enrollAgent(t, admin, oldName)
	startAgentWith(t, oldState, oldRoot, "--runtime", "docker")
	waitForNodeReady(t, admin, oldName)
	old := findNode(t, admin, oldName)

	newState, newRoot := enrollAgent(t, admin, newName)
	startAgentWith(t, newState, newRoot, "--runtime", "docker")
	waitForNodeReady(t, admin, newName)
	replacement := findNode(t, admin, newName)

	wsOld := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)
	if wsOld.NodeID == nil || *wsOld.NodeID != old.ID {
		t.Fatalf("Workspace node_id = %v, want the older Node %s (the other is %s)",
			wsOld.NodeID, old.ID, replacement.ID)
	}
	status, data := createSandboxWithWorkspace(t, admin, fmt.Sprintf("retire-%d", suffix), wsOld.ID, "rw", 0)
	if status != http.StatusCreated {
		t.Fatalf("create Sandbox: %d %s", status, data)
	}
	var sbOld sandboxView
	mustJSON(t, data, &sbOld)
	sbOld = waitForSandboxReady(t, admin, sbOld.ID)
	runExecScript(t, admin, sbOld.ID,
		"printf 'retire me\\n' > /workspace/data.txt\nchmod -R a+rwX /workspace")

	cli := newCLIRunner(t, admin)
	// Pause and resume are the admin switch (ADR-0017). A paused Node takes no
	// new Workspace but keeps running Sandboxes.
	cli.run(t, "", "node", "pause", old.ID)
	if got := getNode(t, admin, old.ID); got.Status != "paused" {
		t.Fatalf("Node status after pause = %q, want paused", got.Status)
	}
	cli.run(t, "", "node", "resume", old.ID)
	if got := getNode(t, admin, old.ID); got.Status != "ready" {
		t.Fatalf("Node status after resume = %q, want ready", got.Status)
	}
	cli.run(t, "", "node", "pause", old.ID)

	// A non-terminal Sandbox and Workspace both block deletion.
	status, data = call(t, http.MethodDelete, "/api/v1/nodes/"+old.ID, admin, nil)
	if status != http.StatusConflict {
		t.Fatalf("delete a Node with work = %d %s, want 409", status, data)
	}
	requireProblemCode(t, data, "conflict")

	// The archive: a paused Node still publishes because the Workspace is
	// bound there.
	resourceName := fmt.Sprintf("retire-%d", suffix)
	var version publishedVersion
	mustJSON(t, cli.run(t, "", "workspace", "publish", wsOld.ID, "/workspace", resourceName), &version)
	if version.State != "ready" || version.Address == nil {
		t.Fatalf("published version = %+v, want a ready version with an address", version)
	}

	// With the Sandbox gone the Workspace alone still blocks the Node.
	deleteSandbox(t, admin, sbOld.ID)
	status, data = call(t, http.MethodDelete, "/api/v1/nodes/"+old.ID, admin, nil)
	if status != http.StatusConflict {
		t.Fatalf("delete a Node with a non-terminal Workspace = %d %s, want 409", status, data)
	}
	requireProblemCode(t, data, "conflict")

	// A Workspace already bound to the paused Node can still start a Sandbox
	// (ADR-0017), and a stopped Sandbox is still non-terminal.
	status, data = createSandboxWithWorkspace(t, admin, fmt.Sprintf("retire-stop-%d", suffix), wsOld.ID, "rw", 0)
	if status != http.StatusCreated {
		t.Fatalf("create second Sandbox: %d %s", status, data)
	}
	var sbStopped sandboxView
	mustJSON(t, data, &sbStopped)
	sbStopped = waitForSandboxReady(t, admin, sbStopped.ID)
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+sbStopped.ID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop second Sandbox: %d %s", status, data)
	}
	waitForSandboxState(t, admin, sbStopped.ID, "stopped", 60*time.Second)
	if status, data := call(t, http.MethodDelete, "/api/v1/workspaces/"+wsOld.ID, admin, nil); status != http.StatusAccepted {
		t.Fatalf("delete Workspace: %d %s", status, data)
	}
	waitForWorkspaceState(t, admin, wsOld.ID, "deleted", 60*time.Second)
	status, data = call(t, http.MethodDelete, "/api/v1/nodes/"+old.ID, admin, nil)
	if status != http.StatusConflict {
		t.Fatalf("delete a Node with a stopped Sandbox = %d %s, want 409", status, data)
	}
	requireProblemCode(t, data, "conflict")

	// Restore into a Workspace on the other Node; the paused Node takes no new
	// Workspace, so placement has to choose the other one.
	wsNew := waitForWorkspaceState(t, admin, createWorkspace(t, admin).ID, "ready", 60*time.Second)
	if wsNew.NodeID == nil || *wsNew.NodeID != replacement.ID {
		t.Fatalf("restored Workspace node_id = %v, want %s", wsNew.NodeID, replacement.ID)
	}
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes", admin, map[string]any{
		"name":      fmt.Sprintf("restore-%d", suffix),
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"workspace": map[string]string{"id": wsNew.ID, "mode": "rw"},
		"resources": []map[string]any{{"resource": resourceName, "version": version.ID, "mount": "archive"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create restoring Sandbox: %d %s", status, data)
	}
	var sbNew sandboxView
	mustJSON(t, data, &sbNew)
	sbNew = waitForSandboxReady(t, admin, sbNew.ID)
	// cp -a copies the read-only mount's mode onto /workspace; restore write
	// permission so t.TempDir's cleanup can remove the tree (as the publish
	// scenario does).
	runExecScript(t, admin, sbNew.ID,
		`cp -a /resources/archive/. /workspace/ && chmod -R a+rwX /workspace && test "$(cat /workspace/data.txt)" = "retire me"`)

	// The stopped Sandbox is the last blocker.
	deleteSandbox(t, admin, sbStopped.ID)
	cli.run(t, "", "node", "delete", old.ID)

	// Tombstone: hidden from the Node and capacity views, token cleared, GPU
	// rows removed, row kept.
	status, data = call(t, http.MethodGet, "/api/v1/nodes/"+old.ID, admin, nil)
	if status != http.StatusNotFound {
		t.Fatalf("get a deleted Node = %d %s, want 404", status, data)
	}
	requireProblemCode(t, data, "not_found")
	if status, data := call(t, http.MethodGet, "/api/v1/nodes?limit=100", admin, nil); status != http.StatusOK {
		t.Fatalf("list Nodes: %d %s", status, data)
	} else if bytes.Contains(data, []byte(old.ID)) {
		t.Fatalf("deleted Node %s still appears in GET /nodes", old.ID)
	}
	if status, data := call(t, http.MethodGet, "/api/v1/capacity", admin, nil); status != http.StatusOK {
		t.Fatalf("get capacity: %d %s", status, data)
	} else if bytes.Contains(data, []byte(old.ID)) {
		t.Fatalf("deleted Node %s still appears in GET /capacity", old.ID)
	}
	if n := psqlCount(t, fmt.Sprintf(
		"SELECT count(*) FROM nodes WHERE id = '%s' AND deleted_at IS NOT NULL AND token_hash = ''::bytea",
		old.ID)); n != "1" {
		t.Fatalf("Node tombstone rows = %s, want 1 with a cleared token", n)
	}
	if n := psqlCount(t, fmt.Sprintf("SELECT count(*) FROM gpus WHERE node_id = '%s'", old.ID)); n != "0" {
		t.Fatalf("deleted Node kept %s GPU rows", n)
	}
	// The connected Agent sees UNAUTHENTICATED and clears its identity.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(oldState); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the deleted Node's Agent kept its identity file")
		}
		time.Sleep(250 * time.Millisecond)
	}

	// Every admin act is audited.
	for _, action := range []string{"node.pause", "node.resume", "node.delete"} {
		status, data := call(t, http.MethodGet,
			"/api/v1/audit?action="+action+"&target="+old.ID, admin, nil)
		if status != http.StatusOK {
			t.Fatalf("list audit %s: %d %s", action, status, data)
		}
		var list struct {
			Items []struct {
				Action string `json:"action"`
			} `json:"items"`
		}
		mustJSON(t, data, &list)
		if len(list.Items) == 0 {
			t.Fatalf("no audit record for %s on %s", action, old.ID)
		}
	}

	deleteSandbox(t, admin, sbNew.ID)
}
