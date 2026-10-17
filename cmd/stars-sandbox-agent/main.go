// stars-sandbox-agent enrolls and runs a Node, and doubles as the in-container
// Supervisor (`supervisor init|probe`).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/zibyn/stars-sandbox/internal/agent"
	"github.com/zibyn/stars-sandbox/internal/supervisor"
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
	// the mode set; otherwise `stars-sandbox-agent supervisor <mode>` does.
	if filepath.Base(os.Args[0]) == "supervisor" {
		err = supervise(os.Args[1:])
	} else {
		switch os.Args[1] {
		case "enroll":
			err = enroll(ctx, os.Args[2:])
		case "run":
			err = run(ctx, os.Args[2:])
		case "supervisor":
			err = supervise(os.Args[2:])
		case "init", "probe":
			// runtime.md names the mounted binary `/.stars/bin/supervisor` and calls
			// it as `supervisor init|probe`, so accept the mode on its own too.
			err = supervise(os.Args[1:])
		default:
			usage()
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "stars-sandbox-agent:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage:
  stars-sandbox-agent enroll --server HOST:PORT --packet sse_... [--ca FILE] [--name NAME] [--state FILE]
  stars-sandbox-agent run [--ca FILE] [--server HOST:PORT] [--state FILE] [--docker SOCKET] [--data-root DIR] [--reserve-cpu N] [--reserve-memory BYTES]
  stars-sandbox-agent supervisor init
  stars-sandbox-agent supervisor probe [WORKSPACE]
  stars-sandbox-agent supervisor run EXEC_ID`)
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
	fs.StringVar(&opts.Docker, "docker", agent.DefaultDockerSock, "Docker Engine socket")
	fs.StringVar(&opts.DataRoot, "data-root", agent.DefaultDataRoot, "Workspace and Supervisor data directory")
	fs.Float64Var(&opts.ReserveCPU, "reserve-cpu", agent.DefaultReserveCPU, "CPU cores kept free for the host")
	fs.Int64Var(&opts.ReserveMemory, "reserve-memory", agent.DefaultReserveBytes, "memory bytes kept free for the host")
	fs.Parse(args)
	return agent.Run(ctx, opts)
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
	default:
		usage()
		return nil
	}
}
