package agent

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	nodev1 "github.com/zibyn/orrery/gen/node/v1"
)

// createRecorder is a driver whose only used operation is Create.
type createRecorder struct {
	runtimeDriver
	created chan string
}

func (d *createRecorder) createSandbox(_ context.Context, cmd *createSandboxSpec) (string, error) {
	d.created <- cmd.sandboxID
	return "", nil
}

// #178: commands are served before the self-check ends, but a CreateSandbox
// that arrives early runs only once the Capabilities are known.
func TestCreateSandboxWaitsForSelfCheck(t *testing.T) {
	driver := &createRecorder{created: make(chan string, 1)}
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
		if got != id {
			t.Fatalf("created %s, want %s", got, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Create did not run after the self-check")
	}
}
