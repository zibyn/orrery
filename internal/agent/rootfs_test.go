package agent

import (
	"testing"

	"github.com/moby/sys/mountinfo"
)

// Under runsc the file API reads the shim's rootfs mount, found by the
// containerd snapshot's upperdir among the host's overlay mounts.
func TestOverlayMountOfAnUpperdir(t *testing.T) {
	host := []*mountinfo.Info{
		{Mountpoint: "/", VFSOptions: "rw,errors=remount-ro"},
		{Mountpoint: "/run/c/other/rootfs", VFSOptions: "rw,lowerdir=/s/1/fs,upperdir=/s/20/fs,workdir=/s/20/work"},
		{Mountpoint: "/run/c/sb/rootfs", VFSOptions: "rw,lowerdir=/s/1/fs,upperdir=/s/2/fs,workdir=/s/2/work"},
	}
	if got := overlayMountOf(host, "/s/2/fs"); got != "/run/c/sb/rootfs" {
		t.Fatalf("mount of /s/2/fs = %q", got)
	}
	if got := overlayMountOf(host, "/s/3/fs"); got != "" {
		t.Fatalf("mount of an unmounted upperdir = %q", got)
	}
	if got := overlayMountOf(host, ""); got != "" {
		t.Fatalf("mount of no upperdir = %q", got)
	}
}
