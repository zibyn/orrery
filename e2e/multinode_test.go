//go:build e2e

package e2e

import (
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
