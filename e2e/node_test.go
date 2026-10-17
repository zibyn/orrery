//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type nodeView struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Status        string           `json:"status"`
	RuntimeHealth *string          `json:"runtime_health"`
	Capabilities  map[string]any   `json:"capabilities"`
	GPUs          []map[string]any `json:"gpus"`
}

// TestEnrollNode is the "enroll-node" acceptance scenario (#39 M1 item 2):
// redeem a Packet, report Runtime Capabilities, become schedulable, and refuse
// a second redemption of the same Packet.
func TestEnrollNode(t *testing.T) {
	admin := adminToken(t)
	suffix := time.Now().UnixNano()
	name := fmt.Sprintf("e2e-node-%d", suffix)

	status, data := call(t, http.MethodPost, "/api/v1/enrollment-packets", admin, nil)
	if status != http.StatusCreated {
		t.Fatalf("create Enrollment Packet: %d %s", status, data)
	}
	var packet struct {
		ID        string `json:"id"`
		Packet    string `json:"packet"`
		ExpiresAt string `json:"expires_at"`
	}
	mustJSON(t, data, &packet)
	if !strings.HasPrefix(packet.Packet, "sse_") || packet.ExpiresAt == "" {
		t.Fatalf("Enrollment Packet = %+v", packet)
	}

	state := filepath.Join(t.TempDir(), "identity.json")
	enrolled := agentCommand(t, "enroll",
		"--server", nodeURL(), "--ca", nodeCA(),
		"--packet", packet.Packet, "--name", name, "--state", state)
	out, err := enrolled.CombinedOutput()
	if err != nil {
		t.Fatalf("agent enroll: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("identity was not written: %v", err)
	}
	if !bytes.Contains(raw, []byte("ssn_")) {
		t.Fatalf("identity does not hold a Node Token: %s", raw)
	}

	// The Packet is single-use.
	again := agentCommand(t, "enroll",
		"--server", nodeURL(), "--ca", nodeCA(),
		"--packet", packet.Packet, "--name", name+"-again",
		"--state", filepath.Join(t.TempDir(), "identity.json"))
	if out, err := again.CombinedOutput(); err == nil {
		t.Fatalf("second redemption of the same Packet succeeded:\n%s", out)
	}

	node := findNode(t, admin, name)

	agent := runAgentCommand(t, "--ca", nodeCA(), "--state", state)
	var agentLog bytes.Buffer
	agent.Stdout, agent.Stderr = &agentLog, &agentLog
	if err := agent.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	defer func() {
		_ = agent.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = agent.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = agent.Process.Kill()
		}
	}()

	deadline := time.Now().Add(90 * time.Second)
	for {
		status, data = call(t, http.MethodGet, "/api/v1/nodes/"+node.ID, admin, nil)
		if status != http.StatusOK {
			t.Fatalf("get Node: %d %s", status, data)
		}
		var current nodeView
		mustJSON(t, data, &current)
		if current.Status == "ready" {
			if current.RuntimeHealth == nil || *current.RuntimeHealth != "healthy" {
				t.Fatalf("ready Node runtime_health = %v", current.RuntimeHealth)
			}
			if current.Capabilities == nil {
				t.Fatal("ready Node has no Capabilities")
			}
			if len(current.GPUs) != 0 {
				t.Fatalf("CPU-only Node reports GPUs: %v", current.GPUs)
			}
			break
		}
		if time.Now().After(deadline) {
			caps, _ := json.Marshal(current.Capabilities)
			t.Fatalf("Node never became ready (last status %q, health %v, capabilities %s)\nagent log:\n%s",
				current.Status, current.RuntimeHealth, caps, agentLog.String())
		}
		time.Sleep(500 * time.Millisecond)
	}

	listStatus, listData := call(t, http.MethodGet, "/api/v1/nodes?limit=100", admin, nil)
	if listStatus != http.StatusOK {
		t.Fatalf("list Nodes: %d %s", listStatus, listData)
	}
	var list struct {
		Items []nodeView `json:"items"`
	}
	mustJSON(t, listData, &list)
	found := false
	for _, item := range list.Items {
		if item.ID == node.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("GET /nodes does not contain %s", node.ID)
	}
}

func findNode(t *testing.T, admin, name string) nodeView {
	t.Helper()
	status, data := call(t, http.MethodGet, "/api/v1/nodes?limit=100", admin, nil)
	if status != http.StatusOK {
		t.Fatalf("list Nodes: %d %s", status, data)
	}
	var list struct {
		Items []nodeView `json:"items"`
	}
	mustJSON(t, data, &list)
	for _, item := range list.Items {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("Node %q not found after enrollment", name)
	return nodeView{}
}

func agentCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	return exec.Command(agentPath(t), args...)
}

// runAgentCommand includes the Docker socket when the caller's DOCKER_HOST
// points at a unix socket.
func runAgentCommand(t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	args = append([]string{"run"}, args...)
	if socket := os.Getenv("STARS_E2E_DOCKER_SOCKET"); socket != "" {
		args = append(args, "--docker", socket)
	}
	return exec.Command(agentPath(t), args...)
}

func agentPath(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("STARS_E2E_AGENT"); v != "" {
		return v
	}
	t.Fatal("STARS_E2E_AGENT is not set; run make e2e")
	return ""
}

func nodeURL() string {
	if v := os.Getenv("STARS_E2E_NODE_URL"); v != "" {
		return v
	}
	return "127.0.0.1:18081"
}

func nodeCA() string {
	if v := os.Getenv("STARS_E2E_NODE_CA"); v != "" {
		return v
	}
	return ".e2e/tls/server.crt"
}
