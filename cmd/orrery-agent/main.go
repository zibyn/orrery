// orrery-agent enrolls and runs a Node, and doubles as the in-container
// Supervisor (`supervisor init|probe`).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zibyn/orrery/internal/agent"
	"github.com/zibyn/orrery/internal/resourcefs"
	"github.com/zibyn/orrery/internal/supervisor"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The first SIGTERM exits gracefully; a second one forces (ADR-0014).
	go func() {
		<-ctx.Done()
		stop()
	}()

	var err error
	// Inside a Sandbox the binary is mounted as `supervisor`, so argv[0] picks
	// the mode set; otherwise `orrery-agent supervisor <mode>` does.
	if filepath.Base(os.Args[0]) == "supervisor" {
		err = supervise(os.Args[1:])
	} else {
		switch os.Args[1] {
		case "enroll":
			err = enroll(ctx, os.Args[2:])
		case "run":
			err = run(ctx, os.Args[2:])
		case "resourcefs":
			err = resourceFS(ctx, os.Args[2:])
		case "supervisor":
			err = supervise(os.Args[2:])
		case "init", "probe":
			// runtime.md names the mounted binary `/.orrery/bin/supervisor` and calls
			// it as `supervisor init|probe`, so accept the mode on its own too.
			err = supervise(os.Args[1:])
		default:
			usage()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "orrery-agent:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  orrery-agent enroll --server HOST:PORT --packet ore_... [--ca FILE] [--name NAME] [--state FILE] [--data-root DIR]
  orrery-agent run [--ca FILE] [--server HOST:PORT] [--state FILE] [--runtime containerd|local] [--local-user USER] [--containerd SOCKET] [--isolation runc|runsc] [--sandbox-subnet CIDR] [--buildkit ADDR] [--data-root DIR] [--reserve-cpu N] [--reserve-memory BYTES] [--mesh-port PORT] [--mesh-endpoint HOST:PORT] [--resourcefs-socket PATH] [--presign-socket PATH]
  orrery-agent resourcefs [--data-root DIR] [--socket PATH] [--presign-socket PATH] [--takeover]
  orrery-agent supervisor init
  orrery-agent supervisor probe [WORKSPACE]
  orrery-agent supervisor run EXEC_ID
  orrery-agent supervisor forward PORT SOCKET
  orrery-agent supervisor dial HOST:PORT
  orrery-agent supervisor listen HOST:PORT`)
	os.Exit(2)
}

func enroll(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	var opts agent.EnrollOptions
	fs.StringVar(&opts.Server, "server", "", "Node listener address, host:port")
	fs.StringVar(&opts.Packet, "packet", "", "single-use Enrollment Packet")
	fs.StringVar(&opts.CAPath, "ca", "", "PEM file with the Control Plane CA")
	fs.StringVar(&opts.Name, "name", "", "Node name (default: hostname)")
	fs.StringVar(&opts.State, "state", agent.DefaultStatePath, "identity file")
	fs.StringVar(&opts.DataRoot, "data-root", agent.DefaultDataRoot, "data directory of the Node (its mesh key is cleared)")
	fs.Parse(args)
	id, err := agent.Enroll(ctx, opts)
	if err == nil {
		fmt.Printf("enrolled node %s\n", id.NodeID)
	}
	return err
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	var opts agent.RunOptions
	fs.StringVar(&opts.CAPath, "ca", "", "PEM file with the Control Plane CA")
	fs.StringVar(&opts.Server, "server", "", "override the enrolled node address")
	fs.StringVar(&opts.State, "state", agent.DefaultStatePath, "identity file")
	fs.StringVar(&opts.Runtime, "runtime", agent.ProviderContainerd, "Runtime Provider: containerd or local")
	fs.StringVar(&opts.Containerd, "containerd", agent.DefaultContainerdSock, "containerd socket (--runtime containerd); the default is the Agent's own containerd, override it only for development and tests")
	fs.StringVar(&opts.Isolation, "isolation", "", "Isolation Runtime: runc (default) or runsc (--runtime containerd)")
	fs.StringVar(&opts.SandboxSubnet, "sandbox-subnet", agent.DefaultSandboxSubnet, "this Node's Sandbox subnet, not overlapping other Nodes' (--runtime containerd)")
	fs.StringVar(&opts.LocalUser, "local-user", "", "unprivileged user every Local Runtime Exec runs as (required with --runtime local)")
	fs.StringVar(&opts.Buildkit, "buildkit", agent.DefaultBuildkitAddr, "BuildKit endpoint, a standalone buildkitd; empty turns snapshot builds off")
	fs.StringVar(&opts.DataRoot, "data-root", agent.DefaultDataRoot, "Workspace and Supervisor data directory")
	fs.Float64Var(&opts.ReserveCPU, "reserve-cpu", agent.DefaultReserveCPU, "CPU cores kept free for the host")
	fs.Int64Var(&opts.ReserveMemory, "reserve-memory", agent.DefaultReserveBytes, "memory bytes kept free for the host")
	fs.IntVar(&opts.MeshPort, "mesh-port", agent.DefaultMeshPort, "Node Mesh WireGuard UDP listen port")
	fs.StringVar(&opts.MeshEndpoint, "mesh-endpoint", "", "host:port other Nodes reach this Node's WireGuard at (default: none, only dial out)")
	fs.Float64Var(&opts.DiskWatermark, "disk-watermark", agent.DefaultDiskWatermark, "fraction of disk kept free before new allocations are refused")
	fs.StringVar(&opts.ResourceFSSocket, "resourcefs-socket", resourcefs.DefaultControlSocket, "orrery-resourcefs control socket")
	fs.StringVar(&opts.PresignSocket, "presign-socket", resourcefs.DefaultPresignSocket, "socket the Agent answers orrery-resourcefs presign requests on")
	fs.Parse(args)
	return agent.Run(ctx, opts)
}

// resourceFS runs orrery-resourcefs, the Node's lazy Resource mount daemon
// (runtime.md §11). --takeover takes the mounts over from the running one.
func resourceFS(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("resourcefs", flag.ExitOnError)
	cfg := resourcefs.Config{Log: slog.Default()}
	var presignSocket string
	fs.StringVar(&cfg.DataRoot, "data-root", agent.DefaultDataRoot, "data directory of the Node, as given to orrery-agent run")
	fs.StringVar(&cfg.ControlSocket, "socket", resourcefs.DefaultControlSocket, "control socket the Agent and a successor connect to")
	fs.StringVar(&presignSocket, "presign-socket", resourcefs.DefaultPresignSocket, "the Agent's presign socket")
	fs.BoolVar(&cfg.Takeover, "takeover", false, "take every mount over from the daemon on --socket (upgrade)")
	fs.Parse(args)
	cfg.Presigner = resourcefs.SocketPresigner{Socket: presignSocket}
	if cfg.Takeover && os.Getenv(takeoverChildEnv) == "" {
		return spawnTakeover()
	}
	cfg.Notify = sdNotify
	if cfg.Takeover {
		// Tell the waiting `--takeover` parent once this process serves.
		done := os.NewFile(3, "takeover-done")
		cfg.Notify = func(state string) {
			sdNotify(state)
			if state == "READY=1" && done != nil {
				done.WriteString("ok\n") //nolint:errcheck // the parent then sees EOF and fails
				done.Close()             //nolint:errcheck // one message
				done = nil
			}
		}
	}
	return resourcefs.Run(ctx, cfg)
}

// takeoverChildEnv marks the detached successor `--takeover` starts.
const takeoverChildEnv = "ORRERY_RESOURCEFS_TAKEOVER_CHILD"

// spawnTakeover is systemd's ExecReload: it starts the successor detached in
// the unit's cgroup and exits once the successor serves every mount, as the
// unit's new main process (it sends MAINPID itself). A failure leaves the old
// process serving.
func spawnTakeover() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Env = append(os.Environ(), takeoverChildEnv+"=1")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{w} // fd 3
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	w.Close() //nolint:errcheck // the child holds its copy
	// Before systemd's 90 s reload timeout: a successor that hangs before it
	// told the old process "ready" is killed, and the old one keeps serving.
	timer := time.AfterFunc(80*time.Second, func() { cmd.Process.Kill() }) //nolint:errcheck // already gone is fine
	line, _ := bufio.NewReader(r).ReadString('\n')                         // EOF when the child dies
	timer.Stop()
	if line != "ok\n" {
		cmd.Process.Kill() //nolint:errcheck // already gone is fine
		cmd.Wait()         //nolint:errcheck // report the takeover failure instead
		return errors.New("takeover failed; the running resourcefs keeps serving")
	}
	return cmd.Process.Release()
}

// sdNotify sends one sd_notify(3) state to systemd; outside systemd it does
// nothing.
func sdNotify(state string) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:]
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		slog.Warn("sd_notify", "state", state, "error", err)
		return
	}
	defer conn.Close() //nolint:errcheck // one datagram
	if _, err := conn.Write([]byte(state)); err != nil {
		slog.Warn("sd_notify", "state", state, "error", err)
	}
}

func supervise(args []string) error {
	if len(args) == 0 {
		usage()
	}
	switch args[0] {
	case "init":
		supervisor.Init()
		return nil
	case "probe":
		workspace := "/workspace"
		if len(args) > 1 {
			workspace = args[1]
		}
		return supervisor.WriteProbeJSON(workspace)
	case "run":
		if len(args) != 2 {
			usage()
		}
		return supervisor.Run(args[1])
	case "forward":
		if len(args) != 3 {
			usage()
		}
		return supervisor.Forward(args[1], args[2])
	case "dial":
		if len(args) != 2 {
			usage()
		}
		return supervisor.Dial(args[1])
	case "listen":
		if len(args) != 2 {
			usage()
		}
		return supervisor.Listen(args[1])
	default:
		usage()
		return nil
	}
}
