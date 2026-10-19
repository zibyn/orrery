package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/zibyn/stars-sandbox/internal/api"
	"github.com/zibyn/stars-sandbox/internal/buildinfo"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	args := os.Args[1:]
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	switch command {
	case "serve":
		return serve(args, logger)
	case "bootstrap":
		return runBootstrap(args, logger)
	case "openapi":
		return runOpenAPI(logger)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q (try %s help)", command, os.Args[0])
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage: stars-sandbox-server [command]

commands:
  serve       run the Control Plane (default)
  bootstrap   create the first admin interactively
  openapi     print the generated OpenAPI document

environment:
  STARS_DATABASE_URL            PostgreSQL DSN (required)
  STARS_API_ADDR                listen address (default :8080)
  STARS_DEFAULT_GPU_QUOTA       new User GPU quota (default 0)
  STARS_DEFAULT_SANDBOX_QUOTA   new User Sandbox quota (default 0)
  STARS_APPS_DOMAIN             wildcard domain for port forwarding (off when empty)
  STARS_PORT_TICKET_KEY         signs port tickets and Gateway cookies
  STARS_PLATFORM_SNAPSHOTS      name=ref@sha256:…,… imported at startup (platform images)
  STARS_DEFAULT_SNAPSHOT        platform Snapshot used when a create omits snapshot
  STARS_REGISTRY_ADDR           Node-reachable registry address (e.g. 127.0.0.1:15000)
  STARS_REGISTRY_PUSH_ADDR      Control Plane push address (default STARS_REGISTRY_ADDR)
  STARS_REGISTRY_CREDENTIAL     Docker config.json for Nodes and pushes; list both hosts
  STARS_BUILD_CPU               Snapshot build CPU ceiling (default 4)
  STARS_BUILD_MEMORY            Snapshot build memory ceiling in bytes (default 8 GiB)
  STARS_BUILD_TIMEOUT           Snapshot build timeout (default 1h)
`)
}

func serve(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := api.ConfigFromEnv()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := api.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := api.Migrate(ctx, pool, cfg.DatabaseURL); err != nil {
		return err
	}
	logger.Info("migrations applied")
	server := api.New(cfg, pool, logger)
	logger.Info("listening", "addr", cfg.Addr, "version", buildinfo.Version())
	return server.Serve(ctx)
}

func runBootstrap(args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("bootstrap", flag.ContinueOnError)
	username := fs.String("username", "", "admin username (prompted when omitted)")
	email := fs.String("email", "", "admin email (optional)")
	passwordStdin := fs.Bool("password-stdin", false, "read the password from the first line of stdin")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := api.ConfigFromEnv()
	if err != nil {
		return err
	}
	ctx := context.Background()
	pool, err := api.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := api.Migrate(ctx, pool, cfg.DatabaseURL); err != nil {
		return err
	}

	reader := bufio.NewReader(os.Stdin)
	name := *username
	if name == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fmt.Errorf("--username is required when stdin is not a terminal")
		}
		fmt.Fprint(os.Stderr, "Username [admin]: ")
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		name = strings.TrimSpace(line)
		if name == "" {
			name = "admin"
		}
	}
	adminEmail := *email
	if adminEmail == "" && term.IsTerminal(int(os.Stdin.Fd())) && !*passwordStdin {
		fmt.Fprint(os.Stderr, "Email (optional): ")
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		adminEmail = strings.TrimSpace(line)
	}

	var password string
	switch {
	case *passwordStdin:
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		password = strings.TrimRight(line, "\r\n")
	case term.IsTerminal(int(os.Stdin.Fd())):
		first, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		fmt.Fprint(os.Stderr, "Password (again): ")
		second, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return err
		}
		if string(first) != string(second) {
			return fmt.Errorf("passwords do not match")
		}
		password = string(first)
	default:
		return fmt.Errorf("--password-stdin is required when stdin is not a terminal")
	}

	server := api.New(cfg, pool, logger)
	user, err := server.Bootstrap(ctx, api.BootstrapOptions{
		Username: name,
		Email:    adminEmail,
		Password: password,
	})
	if err != nil {
		return err
	}
	fmt.Printf("created admin %q (%s)\n", user.Name, user.ID)
	return nil
}

func runOpenAPI(logger *slog.Logger) error {
	server := api.New(api.Config{}, nil, logger)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(server.OpenAPIDocument())
}
