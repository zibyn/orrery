package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
	"github.com/zibyn/orrery/internal/nodeproto"
)

// createRecorder is a driver whose only used operation is Create.
type createRecorder struct {
	runtimeDriver
	created chan *createSandboxSpec
}

func (d *createRecorder) createSandbox(_ context.Context, cmd *createSandboxSpec) (string, error) {
	d.created <- cmd
	return "", nil
}

// #178: commands are served before the self-check ends, but a CreateSandbox
// that arrives early runs only once the Capabilities are known.
func TestCreateSandboxWaitsForSelfCheck(t *testing.T) {
	driver := &createRecorder{created: make(chan *createSandboxSpec, 1)}
	rt := newRuntime(ProviderContainerd, t.TempDir(), uuid.NewString())
	rt.driver = driver
	runner := newCommandRunner(t.Context(), rt, func(*nodev1.AgentMessage) {})

	id := uuid.NewString()
	runner.handle(&nodev1.ControlMessage{Message: &nodev1.ControlMessage_CreateSandbox{
		CreateSandbox: &nodev1.CreateSandbox{CommandId: "c1", SandboxId: id, Generation: 1},
	}})
	select {
	case <-driver.created:
		t.Fatal("Create ran before the self-check finished")
	case <-time.After(100 * time.Millisecond):
	}
	runner.selfChecked()
	select {
	case got := <-driver.created:
		if got.sandboxID != id {
			t.Fatalf("created %s, want %s", got.sandboxID, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Create did not run after the self-check")
	}
}

// #195: CreateSandbox.link_target reaches the driver, which records it with
// the veth (node-protocol.md).
func TestCreateSandboxPassesLinkTarget(t *testing.T) {
	driver := &createRecorder{created: make(chan *createSandboxSpec, 1)}
	rt := newRuntime(ProviderContainerd, t.TempDir(), uuid.NewString())
	rt.driver = driver
	runner := newCommandRunner(t.Context(), rt, func(*nodev1.AgentMessage) {})
	runner.selfChecked()
	runner.handle(&nodev1.ControlMessage{Message: &nodev1.ControlMessage_CreateSandbox{
		CreateSandbox: &nodev1.CreateSandbox{CommandId: "c1", SandboxId: uuid.NewString(), Generation: 1, LinkTarget: true},
	}})
	select {
	case got := <-driver.created:
		if !got.linkTarget {
			t.Fatal("link_target was not passed to the driver")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Create did not run")
	}
}

// node-protocol.md: `running` carries the address CNI gave this Create, read
// from the Sandbox's network.json; other phases, and a Sandbox without one
// (Local), carry none.
func TestObserveSandboxAddress(t *testing.T) {
	rt := newRuntime(ProviderContainerd, t.TempDir(), uuid.NewString())
	var got []*nodev1.SandboxObserved
	runner := newCommandRunner(t.Context(), rt, func(msg *nodev1.AgentMessage) {
		got = append(got, msg.GetSandboxObserved())
	})
	id, local := uuid.New(), uuid.New()
	dir := sandboxPath(rt.dataRoot, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "network.json"), []byte(`{"host_veth":"veth1","address":"10.88.0.7"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runner.observeSandbox(id.String(), 1, nodeproto.SandboxPhaseRunning, "")
	runner.observeSandbox(id.String(), 1, nodeproto.SandboxPhaseGone, "")
	runner.observeSandbox(local.String(), 1, nodeproto.SandboxPhaseRunning, "")
	want := []string{"10.88.0.7", "", ""}
	if len(got) != len(want) {
		t.Fatalf("got %d observations, want %d", len(got), len(want))
	}
	for i, obs := range got {
		if obs.GetAddress() != want[i] {
			t.Errorf("observation %d address = %q, want %q", i, obs.GetAddress(), want[i])
		}
	}
}
