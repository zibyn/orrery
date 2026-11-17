//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestVolumeWritesSurviveTheHolder is #278's writable mount end to end
// (runtime.md §17): a Sandbox writes a Volume, a periodic commit makes the
// writes visible through the Volume file API while it runs, the stop commits
// the rest, and a second Sandbox mounting the Volume reads all of it.
func TestVolumeWritesSurviveTheHolder(t *testing.T) {
	admin := adminToken(t)
	raiseSandboxQuota(t, admin, 10)
	name := fmt.Sprintf("e2e-volume-rw-%d", time.Now().UnixNano())
	status, data := call(t, http.MethodPost, "/api/v1/volumes", admin, map[string]any{"name": name})
	if status != http.StatusCreated {
		t.Fatalf("create Volume: %d %s", status, data)
	}
	var volume struct {
		ID string `json:"id"`
	}
	mustJSON(t, data, &volume)
	withVolume := func(sandbox string) string {
		return createReadySandboxWith(t, admin, map[string]any{
			"name": sandbox, "cpu": 0.5, "memory": 256 << 20, "idle_stop": 0,
			"volumes": []map[string]string{{"volume": name, "path": "/data"}},
		})
	}

	writer := withVolume(name + "-w")
	t.Cleanup(func() { deleteSandbox(t, admin, writer) })
	execOutput(t, admin, writer, `set -e
mkdir -p /data/run && printf 'early' > /data/run/early
head -c 9437184 /dev/urandom > /data/run/big && sha256sum /data/run/big | cut -d' ' -f1 > /data/run/big.sum`)
	// The periodic commit shows the writes while the holder runs.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, data = call(t, http.MethodGet, "/api/v1/volumes/"+volume.ID+"/files/content?path=run/early", admin, nil)
		if status == http.StatusOK && string(data) == "early" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no periodic commit within 2 minutes: %d %s", status, data)
		}
		time.Sleep(5 * time.Second)
	}
	execOutput(t, admin, writer, `set -e
printf 'late' > /data/run/late
mv /data/run/early /data/run/renamed && ln /data/run/late /data/run/late.hard`)
	if status, data := call(t, http.MethodPost, "/api/v1/sandboxes/"+writer+"/stop", admin, nil); status != http.StatusAccepted {
		t.Fatalf("stop: %d %s", status, data)
	}
	waitForSandboxState(t, admin, writer, "stopped", 3*time.Minute)

	reader := withVolume(name + "-r")
	t.Cleanup(func() { deleteSandbox(t, admin, reader) })
	got := execOutput(t, admin, reader, `set -e
cat /data/run/renamed /data/run/late /data/run/late.hard; echo
test ! -e /data/run/early
[ "$(sha256sum /data/run/big | cut -d' ' -f1)" = "$(cat /data/run/big.sum)" ] && echo big-ok
stat -c %h /data/run/late`)
	if want := "earlylatelate\nbig-ok\n2\n"; got != want {
		t.Fatalf("the second holder reads %q, want %q", got, want)
	}
	if strings.Contains(got, "early\n") {
		t.Fatal("the renamed file kept its old name")
	}
}
