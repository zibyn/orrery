package agent

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// Runtime Provider names reported in Capabilities (node-protocol.md). One Node
// has exactly one Provider, chosen by Agent configuration (ADR-0014).
const (
	ProviderDocker = "docker"
	ProviderPodman = "podman"
)

// podmanRootlessSocket is the rootless Podman compat API socket for a user
// (runtime.md §9). Podman is supported rootless only.
func podmanRootlessSocket(uid int) string {
	return fmt.Sprintf("/run/user/%d/podman/podman.sock", uid)
}

// podmanUIDFromSocket returns the invoking user's uid when socket is a
// rootless Podman compat socket.
func podmanUIDFromSocket(socket string) (int, bool) {
	podmanDir := filepath.Dir(filepath.Clean(socket))
	userDir := filepath.Dir(podmanDir)
	if filepath.Base(podmanDir) != "podman" ||
		filepath.Base(socket) != "podman.sock" ||
		filepath.Base(userDir) == "." || filepath.Base(userDir) == "/" ||
		filepath.Base(filepath.Dir(userDir)) != "user" ||
		filepath.Dir(filepath.Dir(userDir)) != "/run" {
		return 0, false
	}
	uid, err := strconv.Atoi(filepath.Base(userDir))
	if err != nil || uid <= 0 {
		return 0, false
	}
	return uid, true
}

// rootlessOwner is the host uid/gid that container root maps to under rootless
// Podman. A negative gid leaves the group untouched.
type rootlessOwner struct {
	uid int
	gid int
}

// podmanOwner resolves the uid mapping of a rootless Podman user: container
// root maps to the invoking user's uid and primary gid.
func podmanOwner(uid int) rootlessOwner {
	owner := rootlessOwner{uid: uid, gid: -1}
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil {
		if gid, err := strconv.Atoi(u.Gid); err == nil {
			owner.gid = gid
		}
	}
	return owner
}

// own maps path to the rootless user so the container user can write it. The
// Agent runs as root, so the chown is allowed.
func (o *rootlessOwner) own(path string) error {
	if o == nil {
		return nil
	}
	return os.Chown(path, o.uid, o.gid)
}

// runtimeConfig is the Node's single Provider choice plus whatever the probe
// and the Runtime adapter need from it.
type runtimeConfig struct {
	provider string
	socket   string
	// owner is set for rootless Podman; nil for rootful Docker.
	owner *rootlessOwner
}

func (r *Runtime) config() runtimeConfig {
	return runtimeConfig{provider: r.provider, socket: r.socket, owner: r.owner}
}

// newPodmanRuntime is the explicit constructor for --runtime podman. The
// rootless owner is derived from the socket, which must be the Podman compat
// socket of the invoking user.
func newPodmanRuntime(socket, dataRoot, nodeID string) (*Runtime, error) {
	uid, ok := podmanUIDFromSocket(socket)
	if !ok {
		return nil, fmt.Errorf("rootless Podman socket must be /run/user/<uid>/podman/podman.sock, got %q", socket)
	}
	owner := podmanOwner(uid)
	return newRuntime(runtimeConfig{provider: ProviderPodman, socket: socket, owner: &owner}, dataRoot, nodeID), nil
}
