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
// the rest, and a second Sandbox mounting the Volume reads all of it. It is
// also the "write-attachment-conflict" acceptance scenario (#39 M1 item 14,
// #59, #220, ADR-0031): at most one Sandbox holds a Volume, stopping the
// holder releases it, and a stopped former holder cannot start while another
// holds it. One scenario, so both share one Node.
func TestVolumeWritesSurviveTheHolder(t *testing.T) {
	admin := adminToken(t)
	startAgentNode(t, admin, fmt.Sprintf("e2e-volume-rw-node-%d", time.Now().UnixNano()))
	raiseSandboxQuota(t, admin, 10)
	name := fmt.Sprintf("e2e-volume-rw-%d", time.Now().UnixNano())
	volumeID := createVolume(t, admin, name)
	body := func(sandbox string) map[string]any {
		return map[string]any{
			"name": sandbox, "cpu": 0.5, "memory": 256 << 20, "idle_stop": 0,
			"volumes": []map[string]string{{"volume": name, "path": "/data"}},
		}
	}

	writer := createReadySandboxWith(t, admin, body(name+"-w"))
	t.Cleanup(func() { deleteSandbox(t, admin, writer) })
	status, data := call(t, http.MethodPost, "/api/v1/sandboxes", admin, body(name+"-r"))
	if status != http.StatusConflict {
		t.Fatalf("second holder: %d %s, want 409", status, data)
	}
	requireProblemCode(t, data, "conflict")
	execOutput(t, admin, writer, `set -e
mkdir -p /data/run && printf 'early' > /data/run/early
head -c 9437184 /dev/urandom > /data/run/big && sha256sum /data/run/big | cut -d' ' -f1 > /data/run/big.sum`)
	// The periodic commit shows the writes while the holder runs.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		status, data = call(t, http.MethodGet, "/api/v1/volumes/"+volumeID+"/files/content?path=run/early", admin, nil)
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

	// Stopping the holder released the Volume.
	reader := createReadySandboxWith(t, admin, body(name+"-r"))
	t.Cleanup(func() { deleteSandbox(t, admin, reader) })
	status, data = call(t, http.MethodPost, "/api/v1/sandboxes/"+writer+"/start", admin, nil)
	if status != http.StatusConflict {
		t.Fatalf("start the writer beside the reader: %d %s, want 409", status, data)
	}
	requireProblemCode(t, data, "conflict")
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
