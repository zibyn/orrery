//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
)

// startAgentNode enrolls and starts one Agent, waits for its Node to become
// ready and returns the Node. Cleanup kills the process with the test.
func startAgentNode(t *testing.T, admin, name string, extra ...string) (nodeView, *agentProcess) {
	t.Helper()
	state, dataRoot := enrollAgent(t, admin, name)
	agent := startAgentWith(t, state, dataRoot, extra...)
	waitForNodeReady(t, admin, name)
	return findNode(t, admin, name), agent
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

// TestUnhealthyRuntime is #93's "unhealthy-runtime" and "multi-node-placement"
// scenarios: of three Nodes on one containerd, only the connected, healthy one
// receives the Sandbox. The policy would prefer the older Node on a tie, so the
// healthy Node is enrolled last, after one never started and one whose Runtime
// self-check and heartbeat report unhealthy. The shared Runtime cannot be
// stopped for one Agent, so the unhealthy Agent is given an unreachable
// --containerd socket.
func TestUnhealthyRuntime(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	offlineName := fmt.Sprintf("e2e-offline-node-%d", suffix)
	unhealthyName := fmt.Sprintf("e2e-unhealthy-node-%d", suffix)
	healthyName := fmt.Sprintf("e2e-healthy-node-%d", suffix)

	// Enrollment order fixes created_at: offline is the oldest, healthy the youngest.
	offlineState, offlineRoot := enrollAgent(t, admin, offlineName)
	state, dataRoot := enrollAgent(t, admin, unhealthyName)
	healthy, _ := startAgentNode(t, admin, healthyName)
	offline := findNode(t, admin, offlineName)

	startAgentWith(t, state, dataRoot, "--containerd", filepath.Join(t.TempDir(), "unreachable.sock"))
	unhealthy := waitForNodeHealth(t, admin, unhealthyName, "unavailable", "unhealthy")
	if unhealthy.ID == healthy.ID {
		t.Fatalf("the unhealthy Agent reused Node %s", healthy.ID)
	}
	if got := getNode(t, admin, unhealthy.ID); got.RuntimeHealth == nil || *got.RuntimeHealth != "unhealthy" {
		t.Fatalf("Node %s runtime_health = %v, want unhealthy", unhealthy.ID, got.RuntimeHealth)
	}

	sb := getSandbox(t, admin, createReadySandbox(t, admin, fmt.Sprintf("unhealthy-%d", suffix), 128<<20))
	if sb.NodeID == nil || *sb.NodeID != healthy.ID {
		t.Fatalf("Sandbox node_id = %v, want the healthy Node %s (offline %s, unhealthy %s)",
			sb.NodeID, healthy.ID, offline.ID, unhealthy.ID)
	}
	// Every Runtime instance carries its Node's ownership label (runtime.md §8).
	if got := sandboxContainerLabel(t, sb.ID, "orrery.node"); got != healthy.ID {
		t.Fatalf("container orrery.node = %q, want %q", got, healthy.ID)
	}

	deleteSandbox(t, admin, sb.ID)
	// The older Node still joins cleanly afterwards.
	startAgentWith(t, offlineState, offlineRoot)
	waitForNodeReady(t, admin, offlineName)
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
	out, err := sandboxCommand("containers", "info", "orrery-"+sandboxID).Output()
	if err != nil {
		t.Fatalf("inspect label %s: %v\n%s", key, err, out)
	}
	var info struct{ Labels map[string]string }
	mustJSON(t, out, &info)
	return info.Labels[key]
}

// TestRetireNode is #94's "retire-node" scenario (#39, ADR-0014). There is no
// eviction: a running Sandbox's data leaves a Node the way a caller moves data
// — publish it to a Resource, restore it into a Sandbox on another Node. Only
// a paused Node with no non-terminal Sandbox can be deleted; the row stays as
// a tombstone.
func TestRetireNode(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	suffix := time.Now().UnixNano()
	oldName := fmt.Sprintf("e2e-retire-old-%d", suffix)
	newName := fmt.Sprintf("e2e-retire-new-%d", suffix)

	// The retiring Node is enrolled and started first, so placement prefers it.
	oldState, oldRoot := enrollAgent(t, admin, oldName)
	startAgentWith(t, oldState, oldRoot)
	waitForNodeReady(t, admin, oldName)
	old := findNode(t, admin, oldName)

	newState, newRoot := enrollAgent(t, admin, newName)
	startAgentWith(t, newState, newRoot)
	waitForNodeReady(t, admin, newName)
	replacement := findNode(t, admin, newName)

	sbOld := getSandbox(t, admin, createReadySandbox(t, admin, fmt.Sprintf("retire-%d", suffix), 128<<20))
	if sbOld.NodeID == nil || *sbOld.NodeID != old.ID {
		t.Fatalf("Sandbox node_id = %v, want the older Node %s (the other is %s)",
			sbOld.NodeID, old.ID, replacement.ID)
	}
	runExecScript(t, admin, sbOld.ID,
		"printf 'retire me\\n' > /workspace/data.txt\nchmod -R a+rwX /workspace")

	// A stopped Sandbox only prefers its Node and does not block deleting it
	// (ADR-0031); it is stopped before the pause, which takes no restarts.
	sbStopped := getSandbox(t, admin, createReadySandbox(t, admin, fmt.Sprintf("retire-stop-%d", suffix), 128<<20))
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+sbStopped.ID+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop second Sandbox: %d %s", status, data)
	}
	waitForSandboxState(t, admin, sbStopped.ID, "stopped", 60*time.Second)

	cli := newCLIRunner(t, admin)
	// Pause and resume are the admin switch (ADR-0017). A paused Node takes no
	// new Sandbox but keeps running ones.
	cli.run(t, "", "node", "pause", old.ID)
	if got := getNode(t, admin, old.ID); got.Status != "paused" {
		t.Fatalf("Node status after pause = %q, want paused", got.Status)
	}
	cli.run(t, "", "node", "resume", old.ID)
	if got := getNode(t, admin, old.ID); got.Status != "ready" {
		t.Fatalf("Node status after resume = %q, want ready", got.Status)
	}
	cli.run(t, "", "node", "pause", old.ID)

	// A non-terminal Sandbox blocks deletion.
	status, data := call(t, http.MethodDelete, "/api/v1/nodes/"+old.ID, admin, nil)
	if status != http.StatusConflict {
		t.Fatalf("delete a Node with work = %d %s, want 409", status, data)
	}
	requireProblemCode(t, data, "conflict")

	// The archive: a paused Node still publishes its running Sandbox.
	resourceName := fmt.Sprintf("retire-%d", suffix)
	version := publishSandbox(t, admin, sbOld.ID, ".", resourceName)
	if version.State != "ready" || version.Address == nil {
		t.Fatalf("published version = %+v, want a ready version with an address", version)
	}

	deleteSandbox(t, admin, sbOld.ID)

	// Restore into a Sandbox on the other Node; the paused Node takes no new
	// Sandbox, so placement has to choose the other one.
	sbNew := getSandbox(t, admin, createReadySandboxWith(t, admin, map[string]any{
		"name":      fmt.Sprintf("restore-%d", suffix),
		"cpu":       0.5,
		"memory":    128 << 20,
		"idle_stop": 0,
		"resources": []map[string]any{{"resource": resourceName, "version": version.ID, "mount": "archive"}},
	}))
	if sbNew.NodeID == nil || *sbNew.NodeID != replacement.ID {
		t.Fatalf("restoring Sandbox node_id = %v, want %s", sbNew.NodeID, replacement.ID)
	}
	// cp -a copies the read-only mount's mode onto /workspace; restore write
	// permission so t.TempDir's cleanup can remove the tree (as the publish
	// scenario does).
	runExecScript(t, admin, sbNew.ID,
		`cp -a /resources/archive/. /workspace/ && chmod -R a+rwX /workspace && test "$(cat /workspace/data.txt)" = "retire me"`)

	deleteSandbox(t, admin, sbStopped.ID)

	// A finished build row is terminal: a Node that ever ran a build can still
	// be deleted (api.md §3). This insert stands in for a build whose Sandbox
	// MarkBuildSandboxDone left at stopped.
	if n := psqlCount(t, fmt.Sprintf(`
		WITH b AS (
			INSERT INTO sandboxes
				(id, owner_id, kind, name, state, node_id, snapshot_digest, cpu, memory)
			VALUES (gen_random_uuid(), (SELECT id FROM users LIMIT 1), 'build',
				'e2e-build-%d', 'stopped', '%s', '', 1, 1073741824)
			RETURNING 1
		) SELECT count(*) FROM b`, suffix, old.ID)); n != "1" {
		t.Fatalf("insert a stopped build row: %s", n)
	}
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
		if !pathExists(oldState) {
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
